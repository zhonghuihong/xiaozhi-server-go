package function

import (
	"fmt"

	"github.com/sashabaranov/go-openai"
)

type FunctionRegistry struct {
	functions map[string]openai.Tool
}

func NewFunctionRegistry() *FunctionRegistry {
	return &FunctionRegistry{
		functions: make(map[string]openai.Tool),
	}
}

func (fr *FunctionRegistry) RegisterFunction(name string, function openai.Tool) error {
	if _, exists := fr.functions[name]; exists {
		return fmt.Errorf("function already registered: %s", name)
	}
	fr.functions[name] = function
	return nil
}

func (fr *FunctionRegistry) GetFunction(name string) (openai.Tool, error) {
	if function, exists := fr.functions[name]; exists {
		return function, nil
	}
	return openai.Tool{}, fmt.Errorf("function not found: %s", name)
}

func (fr *FunctionRegistry) GetAllFunctions() []openai.Tool {
	functions := make([]openai.Tool, 0, len(fr.functions))
	for _, function := range fr.functions {
		functions = append(functions, function)
	}
	return functions
}

func (fr *FunctionRegistry) UnregisterAllFunctions() error {
	// Unregister all functions
	for name := range fr.functions {
		delete(fr.functions, name)
	}
	return nil
}

func (fr *FunctionRegistry) UnregisterFunction(name string) error {
	// Unregister a specific function
	if _, exists := fr.functions[name]; exists {
		delete(fr.functions, name)
	} else {
		return fmt.Errorf("function not found: %s", name)
	}
	return nil
}

func (fr *FunctionRegistry) FunctionExists(name string) bool {
	_, exists := fr.functions[name]
	return exists
}
