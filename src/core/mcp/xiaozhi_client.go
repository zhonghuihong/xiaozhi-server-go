package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"xiaozhi-server-go/src/core/utils"

	"github.com/sashabaranov/go-openai"
)

// MCP消息ID常量
const (
	mcpInitializeID = 1 // 初始化消息ID
	mcpToolsListID  = 2 // 工具列表请求ID
	mcpToolCallID   = 3 // 工具调用请求ID

	msgTypeText = 1 // 文本消息类型
)

// XiaoZhiMCPClient MCP客户端
type XiaoZhiMCPClient struct {
	logger     *utils.Logger
	conn       Conn
	tools      []Tool
	ready      bool
	mu         sync.RWMutex
	ctx        context.Context
	cancelFunc context.CancelFunc

	// 用于处理工具调用的响应
	callResults     map[int]chan interface{}
	callResultsLock sync.Mutex
	nextID          int
}

// NewXiaoZhiMCPClient 创建一个新的MCP客户端
func NewXiaoZhiMCPClient(logger *utils.Logger, conn Conn) *XiaoZhiMCPClient {
	return &XiaoZhiMCPClient{
		logger:      logger,
		conn:        conn,
		tools:       make([]Tool, 0),
		ready:       false,
		callResults: make(map[int]chan interface{}),
		nextID:      1,
	}
}

// SetConnection 设置新的连接
func (c *XiaoZhiMCPClient) SetConnection(conn Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn = conn
}

// ResetConnection 重置连接状态
func (c *XiaoZhiMCPClient) ResetConnection() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 重置连接状态但保留工具注册
	c.conn = nil
	c.ready = false

	return nil
}

// Start 启动MCP客户端
func (c *XiaoZhiMCPClient) Start(ctx context.Context) error {
	c.mu.Lock()
	c.ctx, c.cancelFunc = context.WithCancel(ctx)
	c.mu.Unlock()

	// 发送初始化消息
	return c.SendMCPInitializeMessage()
}

// Stop 停止MCP客户端
func (c *XiaoZhiMCPClient) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.cancelFunc != nil {
		c.cancelFunc()
	}

	// 清理资源
	c.ready = false

	// 取消所有未完成的工具调用
	c.callResultsLock.Lock()
	defer c.callResultsLock.Unlock()

	for id, ch := range c.callResults {
		close(ch)
		delete(c.callResults, id)
	}
}

// HasTool 检查是否有指定名称的工具
func (c *XiaoZhiMCPClient) HasTool(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	for _, tool := range c.tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// GetAvailableTools 获取所有可用工具
func (c *XiaoZhiMCPClient) GetAvailableTools() []openai.Tool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result := make([]openai.Tool, 0, len(c.tools))
	for _, tool := range c.tools {
		result = append(result, openai.Tool{
			Type: "function",
			Function: &openai.FunctionDefinition{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters: map[string]interface{}{
					"type":       tool.InputSchema.Type,
					"properties": tool.InputSchema.Properties,
					"required":   tool.InputSchema.Required,
				},
			},
		})
	}
	return result
}

// CallTool 调用指定的工具
func (c *XiaoZhiMCPClient) CallTool(ctx context.Context, name string, args map[string]interface{}) (interface{}, error) {
	if !c.IsReady() {
		return nil, fmt.Errorf("MCP客户端尚未准备就绪")
	}

	if !c.HasTool(name) {
		return nil, fmt.Errorf("工具 %s 不存在", name)
	}

	// 获取下一个ID并创建结果通道
	c.callResultsLock.Lock()
	id := c.nextID
	c.nextID++
	resultCh := make(chan interface{}, 1)
	c.callResults[id] = resultCh
	c.callResultsLock.Unlock()

	// 构造工具调用请求
	mcpMessage := map[string]interface{}{
		"type": "mcp",
		"payload": map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  "tools/call",
			"params": map[string]interface{}{
				"name":      name,
				"arguments": args,
			},
		},
	}

	data, err := json.Marshal(mcpMessage)
	if err != nil {
		// 清理资源
		c.callResultsLock.Lock()
		delete(c.callResults, id)
		c.callResultsLock.Unlock()
		return nil, fmt.Errorf("序列化MCP工具调用请求失败: %v", err)
	}

	c.logger.Info(fmt.Sprintf("发送客户端mcp工具调用请求: %s，参数: %v", name, args))
	err = c.conn.WriteMessage(msgTypeText, data)
	if err != nil {
		// 清理资源
		c.callResultsLock.Lock()
		delete(c.callResults, id)
		c.callResultsLock.Unlock()
		return nil, err
	}

	// 等待响应或超时
	select {
	case result := <-resultCh:
		if err, ok := result.(error); ok {
			return nil, err
		}
		c.logger.Info(fmt.Sprintf("客户端mcp工具调用 %s 成功，结果: %v", name, result))
		//  map[content:[map[text:{"audio_speaker":{"volume":10},"screen":{},"network":{"type":"wifi","ssid":"zgcinnotown","signal":"weak"}} type:text]] isError:false]
		// 将里面的text提取出来
		if resultMap, ok := result.(map[string]interface{}); ok {
			// 先判断isError是否为true
			if isError, ok := resultMap["isError"].(bool); ok && isError {
				if errorMsg, ok := resultMap["error"].(string); ok {
					return nil, fmt.Errorf("工具调用错误: %s", errorMsg)
				}
				return nil, fmt.Errorf("工具调用返回错误，但未提供具体错误信息")
			}
			// 检查content字段是否存在且为非空数组
			if content, ok := resultMap["content"].([]interface{}); ok && len(content) > 0 {
				if textMap, ok := content[0].(map[string]interface{}); ok {
					if text, ok := textMap["text"].(string); ok {
						return text, nil
					}
				}
			}
		}
		return result, nil
	case <-ctx.Done():
		// 上下文取消或超时
		c.callResultsLock.Lock()
		delete(c.callResults, id)
		c.callResultsLock.Unlock()
		return nil, ctx.Err()
	case <-time.After(30 * time.Second):
		// 请求超时
		c.callResultsLock.Lock()
		delete(c.callResults, id)
		c.callResultsLock.Unlock()
		return nil, fmt.Errorf("工具调用请求超时")
	}
}

// IsReady 检查客户端是否已初始化完成并准备就绪
func (c *XiaoZhiMCPClient) IsReady() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ready
}

// SendMCPInitializeMessage 发送MCP初始化消息
func (c *XiaoZhiMCPClient) SendMCPInitializeMessage() error {
	// 构造MCP初始化消息
	mcpMessage := map[string]interface{}{
		"type": "mcp",
		"payload": map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      mcpInitializeID,
			"method":  "initialize",
			"params": map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]interface{}{
					"roots": map[string]interface{}{
						"listChanged": true,
					},
					"sampling": map[string]interface{}{},
				},
				"clientInfo": map[string]interface{}{
					"name":    "XiaozhiClient",
					"version": "1.0.0",
				},
			},
		},
	}

	data, err := json.Marshal(mcpMessage)
	if err != nil {
		return fmt.Errorf("序列化MCP初始化消息失败: %v", err)
	}

	c.logger.Info("发送MCP初始化消息")
	return c.conn.WriteMessage(msgTypeText, data)
}

// SendMCPToolsListRequest 发送MCP工具列表请求
func (c *XiaoZhiMCPClient) SendMCPToolsListRequest() error {
	// 构造MCP工具列表请求
	mcpMessage := map[string]interface{}{
		"type": "mcp",
		"payload": map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      mcpToolsListID, // 使用新的ID
			"method":  "tools/list",
		},
	}

	data, err := json.Marshal(mcpMessage)
	if err != nil {
		return fmt.Errorf("序列化MCP工具列表请求失败: %v", err)
	}

	c.logger.Debug("发送MCP工具列表请求")
	return c.conn.WriteMessage(msgTypeText, data)
}

// SendMCPToolsListContinueRequest 发送带有cursor的MCP工具列表请求
func (c *XiaoZhiMCPClient) SendMCPToolsListContinueRequest(cursor string) error {
	// 构造MCP工具列表请求
	mcpMessage := map[string]interface{}{
		"type": "mcp",
		"payload": map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      mcpToolsListID, // 使用相同的ID
			"method":  "tools/list",
			"params": map[string]interface{}{
				"cursor": cursor,
			},
		},
	}

	data, err := json.Marshal(mcpMessage)
	if err != nil {
		return fmt.Errorf("序列化MCP工具列表请求失败: %v", err)
	}

	c.logger.Info(fmt.Sprintf("发送带cursor的MCP工具列表请求: %s", cursor))
	return c.conn.WriteMessage(msgTypeText, data)
}

// HandleMCPMessage 处理MCP消息
func (c *XiaoZhiMCPClient) HandleMCPMessage(msgMap map[string]interface{}) error {
	//c.logger.Info("处理MCP消息: " + fmt.Sprintf("%v", msgMap))
	// 获取payload
	payload, ok := msgMap["payload"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("MCP消息缺少payload字段")
	}

	// 检查是否有结果字段（response）
	result, hasResult := payload["result"]
	if hasResult {
		// 获取ID，判断是哪个请求的响应
		id, _ := payload["id"].(float64)
		idInt := int(id)

		// 检查是否是工具调用响应
		c.callResultsLock.Lock()
		if resultCh, ok := c.callResults[idInt]; ok {
			resultCh <- result
			delete(c.callResults, idInt)
			c.callResultsLock.Unlock()
			return nil
		}
		c.callResultsLock.Unlock()

		if id == mcpInitializeID { // 如果是初始化响应
			c.logger.Debug("收到MCP初始化响应")

			// 解析服务器信息
			if serverInfo, ok := result.(map[string]interface{})["serverInfo"].(map[string]interface{}); ok {
				name := serverInfo["name"]
				version := serverInfo["version"]
				c.logger.Info(fmt.Sprintf("客户端MCP服务器信息: name=%v, version=%v", name, version))
			}

			// 初始化完成后，请求工具列表
			return c.SendMCPToolsListRequest()
		} else if id == mcpToolsListID { // 如果是tools/list响应
			c.logger.Debug("收到MCP工具列表响应")

			// 解析工具列表
			if toolsData, ok := result.(map[string]interface{}); ok {
				tools, ok := toolsData["tools"].([]interface{})
				if !ok {
					return fmt.Errorf("工具列表格式错误")
				}

				c.logger.Info(fmt.Sprintf("客户端设备支持的工具数量: %d", len(tools)))

				// 解析工具并添加到列表中
				c.mu.Lock()
				for i, tool := range tools {
					toolMap, ok := tool.(map[string]interface{})
					if !ok {
						continue
					}

					// 构造Tool结构体并添加到列表
					name, _ := toolMap["name"].(string)
					desc, _ := toolMap["description"].(string)

					inputSchema := ToolInputSchema{
						Type: "object",
					}

					if schema, ok := toolMap["inputSchema"].(map[string]interface{}); ok {
						if schemaType, ok := schema["type"].(string); ok {
							inputSchema.Type = schemaType
						}

						if properties, ok := schema["properties"].(map[string]interface{}); ok {
							inputSchema.Properties = properties
						}

						if required, ok := schema["required"].([]interface{}); ok {
							inputSchema.Required = make([]string, 0, len(required))
							for _, r := range required {
								if s, ok := r.(string); ok {
									inputSchema.Required = append(inputSchema.Required, s)
								}
							}
						}
					}

					newTool := Tool{
						Name:        name,
						Description: desc,
						InputSchema: inputSchema,
					}

					c.tools = append(c.tools, newTool)
					c.logger.Debug(fmt.Sprintf("客户端工具 #%d: %v", i+1, name))
				}

				// 检查是否需要继续获取下一页工具
				if nextCursor, ok := toolsData["nextCursor"].(string); ok && nextCursor != "" {
					// 如果有下一页，发送带cursor的请求
					c.logger.Info(fmt.Sprintf("有更多工具，nextCursor: %s", nextCursor))
					c.mu.Unlock()
					return c.SendMCPToolsListContinueRequest(nextCursor)
				} else {
					// 所有工具已获取，设置准备就绪标志
					c.ready = true
				}
				c.mu.Unlock()
			}
		}
	} else if method, hasMethod := payload["method"].(string); hasMethod {
		// 处理客户端发起的请求
		c.logger.Info(fmt.Sprintf("收到MCP客户端请求: %s", method))
		// TODO: 实现处理客户端请求的逻辑
	} else if errorData, hasError := payload["error"].(map[string]interface{}); hasError {
		// 处理错误响应
		errorMsg, _ := errorData["message"].(string)
		c.logger.Error(fmt.Sprintf("收到MCP错误响应: %v", errorMsg))

		// 检查是否是工具调用响应
		if id, ok := payload["id"].(float64); ok {
			idInt := int(id)

			c.callResultsLock.Lock()
			if resultCh, ok := c.callResults[idInt]; ok {
				resultCh <- fmt.Errorf("MCP错误: %s", errorMsg)
				delete(c.callResults, idInt)
			}
			c.callResultsLock.Unlock()
		}
	}

	return nil
}

// SendMCPToolCallRequest 发送MCP工具调用请求
func (c *XiaoZhiMCPClient) SendMCPToolCallRequest(toolName string, arguments map[string]interface{}) error {
	// 构造MCP工具调用请求
	mcpMessage := map[string]interface{}{
		"type": "mcp",
		"payload": map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      mcpToolCallID, // 使用新的ID，与前面的请求区分
			"method":  "tools/call",
			"params": map[string]interface{}{
				"name":      toolName,
				"arguments": arguments,
			},
		},
	}

	data, err := json.Marshal(mcpMessage)
	if err != nil {
		return fmt.Errorf("序列化MCP工具调用请求失败: %v", err)
	}

	c.logger.Info(fmt.Sprintf("发送工具调用请求: %s，参数: %v", toolName, arguments))
	return c.conn.WriteMessage(msgTypeText, data)
}
