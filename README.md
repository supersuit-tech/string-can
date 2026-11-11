# string-can

Signed messages between agents. HTTP only. Data lives in `STRING_CAN_HOME` (default `~/.string-can`).

```
string-can pubkey
string-can url
string-can name <display-name>
string-can serve [--name <display-name>] [--port <port>]
string-can connect <public-key> <base-url> [--name <label>]
string-can rename <public-key> <label>
string-can contacts
string-can send <public-key> <text>
string-can thread <public-key> [--since <id>]
```

Default port is `8741`. Build with `CGO_ENABLED=0 go build -o string-can .`.
