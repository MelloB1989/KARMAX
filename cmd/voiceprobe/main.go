// voiceprobe drives the voice brain the way an integration does — the same
// protocol over the same socket — without placing a call. It is how the
// conversation can be exercised, timed and regressed without ringing anybody.
//
//	go run ./cmd/voiceprobe "Who is Siva?" -wait 60 "bye"
//
// Each argument is spoken as one utterance; "-wait N" lingers N seconds so a
// handed-off task can come back as an unprompted say. A hangup from the brain
// ends the probe the way it ends a call.
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://127.0.0.1:9090/voice", nil)
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer conn.CloseNow()

	send := func(m map[string]any) {
		data, _ := json.Marshal(m)
		if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
			fmt.Println("write:", err)
			os.Exit(1)
		}
	}
	// One reader, one channel: a Read with a timeout would close the socket.
	in := make(chan map[string]any, 16)
	go func() {
		defer close(in)
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			_ = json.Unmarshal(data, &m)
			in <- m
		}
	}()
	recv := func(within time.Duration) (map[string]any, bool) {
		select {
		case m, ok := <-in:
			return m, ok
		case <-time.After(within):
			return nil, false
		}
	}

	send(map[string]any{"type": "start", "call_id": "probe", "peer": "5794649083972@lid",
		"peer_name": "Kartik Deshmukh", "language": "en-IN", "direction": "incoming"})
	if g, ok := recv(15 * time.Second); ok {
		fmt.Printf("greeting: %v\n", g["text"])
	}

	id := int64(0)
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		if args[i] == "-wait" && i+1 < len(args) {
			secs, _ := time.ParseDuration(args[i+1] + "s")
			i++
			deadline := time.Now().Add(secs)
			for time.Now().Before(deadline) {
				m, ok := recv(time.Until(deadline))
				if !ok {
					break
				}
				fmt.Printf("[unprompted] %v -> %v\n", m["type"], m["text"])
				if m["type"] == "hangup" {
					fmt.Println("brain hung up")
					return
				}
			}
			continue
		}
		id++
		start := time.Now()
		send(map[string]any{"type": "utterance", "id": id, "text": args[i]})
		reply, ok := recv(60 * time.Second)
		if !ok {
			fmt.Println("no reply")
			return
		}
		fmt.Printf("[%5.2fs] %q -> (for %v) %v\n", time.Since(start).Seconds(), args[i], reply["for"], reply["text"])
		// A hangup follows its goodbye as a second frame.
		if m, ok := recv(400 * time.Millisecond); ok {
			if m["type"] == "hangup" {
				fmt.Println("brain hung up")
				return
			}
			fmt.Printf("[unprompted] %v -> %v\n", m["type"], m["text"])
		}
	}
	send(map[string]any{"type": "ended", "reason": "probe done"})
}
