package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"xiaozhi-server-go/src/core/chat"
	"xiaozhi-server-go/src/core/image"
	"xiaozhi-server-go/src/core/providers"
	"xiaozhi-server-go/src/core/utils"
	"xiaozhi-server-go/src/task"
)

// handleMessage 处理接收到的消息
func (h *ConnectionHandler) handleMessage(messageType int, message []byte) error {
	switch messageType {
	case 1: // 文本消息
		h.clientTextQueue <- string(message)
		return nil
	case 2: // 二进制消息（音频数据）
		if h.clientAudioFormat == "pcm" {
			// 直接将PCM数据放入队列
			h.clientAudioQueue <- message
		} else if h.clientAudioFormat == "opus" {
			// 检查是否初始化了opus解码器
			if h.opusDecoder != nil {
				// 解码opus数据为PCM
				decodedData, err := h.opusDecoder.Decode(message)
				if err != nil {
					h.logger.Error(fmt.Sprintf("解码Opus音频失败: %v", err))
					// 即使解码失败，也尝试将原始数据传递给ASR处理
					h.clientAudioQueue <- message
				} else {
					// 解码成功，将PCM数据放入队列
					h.logger.Debug(fmt.Sprintf("Opus解码成功: %d bytes -> %d bytes", len(message), len(decodedData)))
					if len(decodedData) > 0 {
						h.clientAudioQueue <- decodedData
					}
				}
			} else {
				// 没有解码器，直接传递原始数据
				h.clientAudioQueue <- message
			}
		}
		return nil
	default:
		h.logger.Error(fmt.Sprintf("未知的消息类型: %d", messageType))
		return fmt.Errorf("未知的消息类型: %d", messageType)
	}
}

// processClientTextMessage 处理文本数据
func (h *ConnectionHandler) processClientTextMessage(ctx context.Context, text string) error {
	// 解析JSON消息
	var msgJSON interface{}
	if err := json.Unmarshal([]byte(text), &msgJSON); err != nil {
		return h.conn.WriteMessage(1, []byte(text))
	}

	// 检查是否为整数类型
	if _, ok := msgJSON.(float64); ok {
		return h.conn.WriteMessage(1, []byte(text))
	}

	// 解析为map类型处理具体消息
	msgMap, ok := msgJSON.(map[string]interface{})
	if !ok {
		return fmt.Errorf("消息格式错误")
	}

	// 根据消息类型分发处理
	msgType, ok := msgMap["type"].(string)
	if !ok {
		return fmt.Errorf("消息类型错误")
	}

	switch msgType {
	case "hello":
		return h.handleHelloMessage(msgMap)
	case "abort":
		return h.clientAbortChat()
	case "listen":
		return h.handleListenMessage(msgMap)
	case "iot":
		return h.handleIotMessage(msgMap)
	case "chat":
		return h.handleChatMessage(ctx, text)
	case "vision":
		return h.handleVisionMessage(msgMap)
	case "image":
		return h.handleImageMessage(ctx, msgMap)
	case "mcp":
		return h.mcpManager.HandleXiaoZhiMCPMessage(msgMap)
	default:
		return fmt.Errorf("未知的消息类型: %s", msgType)
	}
}

func (h *ConnectionHandler) handleVisionMessage(msgMap map[string]interface{}) error {
	// 处理视觉消息
	cmd := msgMap["cmd"].(string)
	if cmd == "gen_pic" {
		text := msgMap["text"].(string)
		params := map[string]interface{}{
			"prompt":    text,
			"size":      "1024x1024",
			"quality":   "standard",
			"api_key":   h.config.LLM["ChatGLMLLM"].APIKey,
			"client_id": h.sessionID,
		}
		task, id := task.NewTask(task.TaskTypeImageGen, params, task.NewMessageCallback(h.conn, "vision", cmd))
		h.taskMgr.SubmitTask(h.sessionID, task)
		h.logger.Info(fmt.Sprintf("生成图片任务提交成功: %s, %s", text, id))
	} else if cmd == "gen_video" {
	} else if cmd == "read_img" {
	}
	return nil
}

// handleHelloMessage 处理欢迎消息
// 客户端会上传语音格式和采样率等信息
func (h *ConnectionHandler) handleHelloMessage(msgMap map[string]interface{}) error {
	h.logger.Info("收到客户端欢迎消息: " + fmt.Sprintf("%v", msgMap))
	// 获取客户端编码格式
	if audioParams, ok := msgMap["audio_params"].(map[string]interface{}); ok {
		if format, ok := audioParams["format"].(string); ok {
			h.logger.Info("客户端音频格式: " + format)
			h.clientAudioFormat = format
			if format == "pcm" {
				// 客户端使用PCM格式，服务端也使用PCM格式
				h.serverAudioFormat = "pcm"
				h.sendHelloMessage()
			}
		}
		if sampleRate, ok := audioParams["sample_rate"].(float64); ok {
			h.logger.Info("客户端采样率: " + fmt.Sprintf("%d", int(sampleRate)))
			h.clientAudioSampleRate = int(sampleRate)
		}
		if channels, ok := audioParams["channels"].(float64); ok {
			h.logger.Info("客户端声道数: " + fmt.Sprintf("%d", int(channels)))
			h.clientAudioChannels = int(channels)
		}
		if frameDuration, ok := audioParams["frame_duration"].(float64); ok {
			h.logger.Info("客户端帧时长: " + fmt.Sprintf("%d", int(frameDuration)))
			h.clientAudioFrameDuration = int(frameDuration)
		}
	}

	h.closeOpusDecoder()
	// 初始化opus解码器
	opusDecoder, err := utils.NewOpusDecoder(&utils.OpusDecoderConfig{
		SampleRate:  h.clientAudioSampleRate, // 客户端使用24kHz采样率
		MaxChannels: h.clientAudioChannels,   // 单声道音频
	})
	if err != nil {
		h.logger.Error(fmt.Sprintf("初始化Opus解码器失败: %v", err))
	} else {
		h.opusDecoder = opusDecoder
		h.logger.Info("Opus解码器初始化成功")
	}

	return nil
}

// handleListenMessage 处理语音相关消息
func (h *ConnectionHandler) handleListenMessage(msgMap map[string]interface{}) error {

	// 处理state参数
	state, ok := msgMap["state"].(string)
	if !ok {
		return fmt.Errorf("listen消息缺少state参数")
	}

	// 处理mode参数
	if mode, ok := msgMap["mode"].(string); ok {
		h.clientListenMode = mode
		h.logger.Info(fmt.Sprintf("客户端拾音模式：%s， %s", h.clientListenMode, state))
		h.providers.asr.SetListener(h)
	}

	switch state {
	case "start":
		if h.client_asr_text != "" && h.clientListenMode == "manual" {
			h.clientAbortChat()
		}
		h.clientVoiceStop = false
		h.client_asr_text = ""
	case "stop":
		h.clientVoiceStop = true
		h.logger.Info("客户端停止语音识别")
	case "detect":
		// 检查是否包含图片数据
		imageBase64, hasImage := msgMap["image"].(string)
		text, hasText := msgMap["text"].(string)

		if hasImage && imageBase64 != "" {
			// 包含图片数据，使用VLLLM处理
			h.logger.Info("检测到客户端发送的图片数据，使用VLLLM处理", map[string]interface{}{
				"has_text":     hasText,
				"text_length":  len(text),
				"image_length": len(imageBase64),
			})

			// 如果没有文本，提供默认提示
			if !hasText || text == "" {
				text = "请描述这张图片"
			}

			// 构造图片数据结构
			imageData := image.ImageData{
				Data:   imageBase64,
				Format: "jpg", // 默认格式，实际格式会在验证时自动检测
			}

			// 调用图片处理逻辑
			return h.handleImageWithText(context.Background(), imageData, text)

		} else if hasText && text != "" {
			// 只有文本，使用普通LLM处理
			h.logger.Info("检测到纯文本消息，使用LLM处理", map[string]interface{}{
				"text": text,
			})
			return h.handleChatMessage(context.Background(), text)
		} else {
			// 既没有图片也没有文本
			h.logger.Warn("detect消息既没有text也没有image参数")
			return fmt.Errorf("detect消息缺少text或image参数")
		}
	}
	return nil
}

// handleImageWithText 处理包含图片和文本的消息
func (h *ConnectionHandler) handleImageWithText(ctx context.Context, imageData image.ImageData, text string) error {
	// 增加对话轮次
	h.talkRound++
	currentRound := h.talkRound
	h.logger.Info(fmt.Sprintf("开始新的图片对话轮次: %d", currentRound))

	// 判断是否需要验证
	if h.isNeedAuth() {
		if err := h.checkAndBroadcastAuthCode(); err != nil {
			h.logger.Error(fmt.Sprintf("检查认证码失败: %v", err))
			return err
		}
		h.logger.Info("设备未认证，等待管理员认证")
		return nil
	}

	// 检查是否有VLLLM Provider
	if h.providers.vlllm == nil {
		h.logger.Warn("未配置VLLLM服务，图片消息将降级为文本处理")
		return h.handleChatMessage(ctx, text+" (注：无法处理图片，仅处理文本)")
	}

	h.logger.Info("开始处理图片+文本消息", map[string]interface{}{
		"text":        text,
		"has_data":    imageData.Data != "",
		"data_length": len(imageData.Data),
	})

	// 立即发送STT消息
	err := h.sendSTTMessage(text)
	if err != nil {
		h.logger.Error(fmt.Sprintf("发送STT消息失败: %v", err))
		return fmt.Errorf("发送STT消息失败: %v", err)
	}

	// 发送TTS开始状态
	if err := h.sendTTSMessage("start", "", 0); err != nil {
		h.logger.Error(fmt.Sprintf("发送TTS开始状态失败: %v", err))
		return fmt.Errorf("发送TTS开始状态失败: %v", err)
	}

	// 立即给用户语音反馈，提升用户体验
	immediateResponse := "正在识别图片请稍等，这就为你分析"
	h.logger.Info("立即播放图片识别提示音", map[string]interface{}{
		"response": immediateResponse,
	})

	// 重置语音状态，确保能够播放提示音
	atomic.StoreInt32(&h.serverVoiceStop, 0)

	// 立即合成并播放提示音（使用索引0确保优先播放）
	if err := h.SpeakAndPlay(immediateResponse, 0, currentRound); err != nil {
		h.logger.Error(fmt.Sprintf("播放图片识别提示音失败: %v", err))
	}

	// 发送思考状态的情绪
	if err := h.sendEmotionMessage("thinking"); err != nil {
		h.logger.Error(fmt.Sprintf("发送思考状态情绪消息失败: %v", err))
		return fmt.Errorf("发送情绪消息失败: %v", err)
	}

	// 添加用户消息到对话历史（包含图片信息的描述）
	userMessage := fmt.Sprintf("%s [用户发送了一张图片]", text)
	h.dialogueManager.Put(chat.Message{
		Role:    "user",
		Content: userMessage,
	})

	// 获取对话历史（排除当前图片消息）
	messages := make([]providers.Message, 0)
	for _, msg := range h.dialogueManager.GetLLMDialogue() {
		if msg.Role == "user" && strings.Contains(msg.Content, "[用户发送了一张图片]") {
			continue
		}
		messages = append(messages, providers.Message{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}

	// 使用VLLLM处理图片消息
	return h.genResponseByVLLM(ctx, messages, imageData, text, currentRound)
}

// handleIotMessage 处理IOT设备消息
func (h *ConnectionHandler) handleIotMessage(msgMap map[string]interface{}) error {
	if descriptors, ok := msgMap["descriptors"].([]interface{}); ok {
		// 处理设备描述符
		// 这里需要实现具体的IOT设备描述符处理逻辑
		h.logger.Info(fmt.Sprintf("收到IOT设备描述符：%v", descriptors))
	}
	if states, ok := msgMap["states"].([]interface{}); ok {
		// 处理设备状态
		// 这里需要实现具体的IOT设备状态处理逻辑
		h.logger.Info(fmt.Sprintf("收到IOT设备状态：%v", states))
	}
	return nil
}

// handleImageMessage 处理图片消息
func (h *ConnectionHandler) handleImageMessage(ctx context.Context, msgMap map[string]interface{}) error {
	// 增加对话轮次
	h.talkRound++
	currentRound := h.talkRound
	h.logger.Info(fmt.Sprintf("开始新的图片对话轮次: %d", currentRound))

	// 判断是否需要验证
	if h.isNeedAuth() {
		if err := h.checkAndBroadcastAuthCode(); err != nil {
			h.logger.Error(fmt.Sprintf("检查认证码失败: %v", err))
			return err
		}
		h.logger.Info("设备未认证，等待管理员认证")
		return nil
	}

	// 检查是否有VLLLM Provider
	if h.providers.vlllm == nil {
		h.logger.Warn("未配置VLLLM服务，图片消息将被忽略")
		return h.conn.WriteMessage(1, []byte("系统暂不支持图片处理功能"))
	}

	// 解析文本内容
	text, ok := msgMap["text"].(string)
	if !ok {
		text = "请描述这张图片" // 默认提示
	}

	// 解析图片数据
	imageDataMap, ok := msgMap["image_data"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("缺少图片数据")
	}

	imageData := image.ImageData{}
	if url, ok := imageDataMap["url"].(string); ok {
		imageData.URL = url
	}
	if data, ok := imageDataMap["data"].(string); ok {
		imageData.Data = data
	}
	if format, ok := imageDataMap["format"].(string); ok {
		imageData.Format = format
	}

	// 验证图片数据
	if imageData.URL == "" && imageData.Data == "" {
		return fmt.Errorf("图片数据为空")
	}

	h.logger.Info("收到图片消息", map[string]interface{}{
		"text":        text,
		"has_url":     imageData.URL != "",
		"has_data":    imageData.Data != "",
		"format":      imageData.Format,
		"data_length": len(imageData.Data),
	})

	// 立即发送STT消息
	err := h.sendSTTMessage(text)
	if err != nil {
		h.logger.Error(fmt.Sprintf("发送STT消息失败: %v", err))
		return fmt.Errorf("发送STT消息失败: %v", err)
	}

	// 发送TTS开始状态
	if err := h.sendTTSMessage("start", "", 0); err != nil {
		h.logger.Error(fmt.Sprintf("发送TTS开始状态失败: %v", err))
		return fmt.Errorf("发送TTS开始状态失败: %v", err)
	}

	// 发送思考状态的情绪
	if err := h.sendEmotionMessage("thinking"); err != nil {
		h.logger.Error(fmt.Sprintf("发送思考状态情绪消息失败: %v", err))
		return fmt.Errorf("发送情绪消息失败: %v", err)
	}

	// 添加用户消息到对话历史（包含图片信息的描述）
	userMessage := fmt.Sprintf("%s [用户发送了一张%s格式的图片]", text, imageData.Format)
	h.dialogueManager.Put(chat.Message{
		Role:    "user",
		Content: userMessage,
	})

	// 获取对话历史
	messages := make([]providers.Message, 0)
	for _, msg := range h.dialogueManager.GetLLMDialogue() {
		// 排除包含图片信息的最后一条消息，因为我们要用VLLLM处理
		if msg.Role == "user" && strings.Contains(msg.Content, "[用户发送了一张") {
			continue
		}
		messages = append(messages, providers.Message{
			Role:    msg.Role,
			Content: msg.Content,
		})
	}

	return h.genResponseByVLLM(ctx, messages, imageData, text, currentRound)
}
