package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"

	"github.com/picatz/websocket"
)

func rpcHandler(w http.ResponseWriter, r *http.Request) {
	// Upgrade the HTTP connection to a WebSocket connection
	conn, err := websocket.Upgrade(w, r)
	if err != nil {
		log.Printf("Upgrade failed: %v", err)
		switch {
		case errors.Is(err, websocket.ErrOriginNotAllowed):
			http.Error(w, "WebSocket origin not allowed", http.StatusForbidden)
		case errors.Is(err, websocket.ErrUnsupportedVersion):
			w.Header().Set("Sec-WebSocket-Version", "13")
			http.Error(w, "Unsupported WebSocket version", http.StatusUpgradeRequired)
		case errors.Is(err, websocket.ErrNotHijacker):
			http.Error(w, "WebSocket upgrade unavailable", http.StatusInternalServerError)
		case !errors.Is(err, websocket.ErrHandshakeFailed):
			// A failed hijack/101 write may already own the transport.
			http.Error(w, "Invalid WebSocket handshake", http.StatusBadRequest)
		}
		return
	}
	defer conn.Close()

	// Handle the WebSocket connection
	for {
		// Read a message from the client
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if err == io.EOF {
				log.Println("Connection closed by peer")
			} else {
				log.Printf("ReadMessage failed: %v", err)
			}
			break
		}

		if messageType != websocket.BinaryMessage {
			log.Printf("Received non-binary message: %s", message)
			continue
		}

		// Unmarshal the message JSON
		var req struct {
			Method string            `json:"method"`
			Params map[string]string `json:"params"`
		}

		err = json.Unmarshal(message, &req)
		if err != nil {
			log.Printf("Unmarshal failed: %v", err)
			continue
		}

		switch req.Method {
		case "sum":
			var resp struct {
				Result int      `json:"result,omitempty"`
				Errors []string `json:"error,omitempty"`
			}

			a, ok := req.Params["a"]
			if !ok {
				resp.Errors = append(resp.Errors, "Missing 'a' parameter")
			}

			b, ok := req.Params["b"]
			if !ok {
				resp.Errors = append(resp.Errors, "Missing 'b' parameter")
			}

			aInt, err := strconv.Atoi(a)
			if err != nil {
				resp.Errors = append(resp.Errors, "Invalid 'a' parameter")
			}

			bInt, err := strconv.Atoi(b)
			if err != nil {
				resp.Errors = append(resp.Errors, "Invalid 'b' parameter")
			}

			if len(resp.Errors) == 0 {
				resp.Result = aInt + bInt
			}

			respJSON, err := json.Marshal(resp)
			if err != nil {
				log.Printf("Marshal failed: %v", err)
				continue
			}

			if err := conn.WriteMessage(websocket.BinaryMessage, respJSON); err != nil {
				log.Printf("WriteMessage failed: %v", err)
				break
			}
		default:
			var resp struct {
				Errors []string `json:"error"`
			}

			resp.Errors = append(resp.Errors, "Unknown method: "+req.Method)

			respJSON, err := json.Marshal(resp)
			if err != nil {
				log.Printf("Marshal failed: %v", err)
				continue
			}

			if err := conn.WriteMessage(websocket.BinaryMessage, respJSON); err != nil {
				log.Printf("WriteMessage failed: %v", err)
				break
			}
		}
	}
}

func main() {
	http.HandleFunc("/ws", rpcHandler)

	log.Println("WebSocket server started on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
