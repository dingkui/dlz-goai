package main

import (
	"context"
	"fmt"
	"log"

	dlzgoai "github.com/dingkui/dlz-goai"
	"github.com/dingkui/dlz-goai/message"
	"github.com/dingkui/dlz-goai/provider/ollama"
)

func main() {
	provider := ollama.New("")
	client, err := dlzgoai.NewClient(dlzgoai.Options{
		Model: func(ctx context.Context, msgs []message.Message, opts *message.Options, emit func(message.Delta)) error {
			return provider.ChatStream(ctx, "qwen3:8b", msgs, opts, emit)
		},
	})
	if err != nil {
		log.Print(err)
		return
	}
	defer client.Close()

	run, err := client.Start(context.Background(), dlzgoai.Request{Input: "用一句话介绍 Go。"})
	if err != nil {
		log.Print(err)
		return
	}
	result, err := run.Wait(context.Background())
	if err != nil {
		log.Print(err)
		return
	}
	fmt.Println(result.Content)
}
