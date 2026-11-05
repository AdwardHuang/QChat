## 增加图标并打包
```shell
go-winres simply --arch amd64 --icon assets/QChat_128x128.ico --manifest gui --file-description "QChat 即时通讯服务" --product-name "QChat"

go build -ldflags="-H=windowsgui" -o QChat.exe .
```