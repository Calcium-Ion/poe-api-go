# Golang Poe API
https://github.com/ading2210/poe-api 的golang版本

# Notice
在 https://github.com/lwydyby/poe-api, 在此基础上进行了一些修改，并修复了一些bug  

# Installation

```bash
go get github.com/Calcium-Ion/poe-api-go
```

```golang
import (
    "github.com/Calcium-Ion/poe-api-go"
)
```

# Documentation

## Create Client

```golang
client, err := poe_api.NewClient("your p-b", "your formkey", nil)
if err != nil {
	log.Printf("failed to create client: %v", err)
}
```

## Sending message

streaming mode

```golang 
res, err := client.SendMessage("ChatGPT", "Ping", true, 30*time.Second)
if err != nil {
    log.Printf("failed to send message: %v", err)
}
for m := range poe_api.GetTextStream(res) {
    fmt.Println(m)
}
```

non-streaming mode

```golang
res, err := client.SendMessage("ChatGPT", "一句话描述golang的channel", true, 30*time.Second)
if err != nil {
    log.Printf("failed to send message: %v", err)
}
fmt.Println(poe_api.GetFinalResponse(res))
```
