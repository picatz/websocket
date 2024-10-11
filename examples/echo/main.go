package main

import (
	"io"
	"log"
	"net/http"

	"github.com/picatz/websocket"
)

func echoHandler(w http.ResponseWriter, r *http.Request) {
	// Upgrade the HTTP connection to a WebSocket connection
	conn, err := websocket.Upgrade(w, r)
	if err != nil {
		log.Printf("Upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	// Handle the WebSocket connection
	for {
		// Read a message
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if err == io.EOF {
				log.Println("Connection closed by peer")
			} else {
				log.Printf("ReadMessage failed: %v", err)
			}
			break
		}

		log.Printf("Received message: %s", message)

		// Echo the message back to the client
		if err := conn.WriteMessage(messageType, message); err != nil {
			log.Printf("WriteMessage failed: %v", err)
			break
		}
	}
}

func main() {
	http.HandleFunc("/ws", echoHandler)

	log.Println("WebSocket server started on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
