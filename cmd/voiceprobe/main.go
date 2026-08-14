// voiceprobe drives the brain protocol the way an integration does, so the
// conversation can be exercised without placing a call.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/coder/websocket"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://127.0.0.1:9090/voice", nil)
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer conn.CloseNow()

	send := func(m map[string]any) {
		data, _ := json.Marshal(m)
		_ = conn.Write(ctx, websocket.MessageText, data)
	}
	recv := func() map[string]any {
		_, data, err := conn.Read(ctx)
		if err != nil {
			fmt.Println("read:", err)
			os.Exit(1)
		}
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		return m
	}

	send(map[string]any{"type": "start", "call_id": "probe", "peer": "5794649083972@lid",
		"peer_name": "Kartik Deshmukh", "language": "en-IN", "direction": "incoming"})
	greeting := recv()
	fmt.Printf("greeting: %v\n", greeting["text"])

	for _, q := range os.Args[1:] {
		start := time.Now()
		send(map[string]any{"type": "utterance", "text": q})
		reply := recv()
		fmt.Printf("[%5.2fs] %q -> %v\n", time.Since(start).Seconds(), q, reply["text"])
	}
	send(map[string]any{"type": "ended", "reason": "probe done"})
}
