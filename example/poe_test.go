package example

import (
	"fmt"
	poe_api "github.com/Calcium-Ion/poe-api-go"
	"log"
	"time"
)

func ExampleSendMessage() {
	client, err := poe_api.NewClient("IkjwrCPgQ-Ex9GPDGhln_w%3D%3D", "3773d0b6f54c8731f084c50c14dae014", nil)
	if err != nil {
		log.Printf("failed to create client: %v", err)
	}

	// streaming mod
	res, err := client.SendMessage("ChatGPT", "Ping", true, 30*time.Second)
	if err != nil {
		panic(err)
	}
	for m := range poe_api.GetTextStream(res) {
		fmt.Println(m)
	}
}
