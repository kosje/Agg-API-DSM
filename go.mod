module aggapi

go 1.24.1


// 依赖取舍（三条都是标准库无法替代的）：
//
//   gorilla/websocket   Copilot 上游走 WebSocket 长连接，标准库没有客户端实现
//   ledongthuc/pdf      PDF 文本提取。自己写会掉进字体编码与内容流的坑，
//                       做不好还会静默输出乱码 —— 那比明确说「不支持」更糟
//   xuri/excelize/v2    XLSX 解析。固定在 v2.9.0：v2.11 要求 go >= 1.25
//
// Go 版本取 1.24：ledongthuc/pdf 要求 >= 1.24.1。
// go.mod 里**不要**写 toolchain 指令 —— 受限网络下它会触发工具链自动下载
// 并失败；用 GOTOOLCHAIN=local + 手动装好的工具链更可靠。

require (
	github.com/gorilla/websocket v1.5.3
	github.com/ledongthuc/pdf v0.0.0-20260907135840-6c8c28e0e8a0
	github.com/xuri/excelize/v2 v2.9.0
)

require (
	github.com/mohae/deepcopy v0.0.0-20170929034955-c48cc78d4826 // indirect
	github.com/richardlehane/mscfb v1.0.4 // indirect
	github.com/richardlehane/msoleps v1.0.4 // indirect
	github.com/xuri/efp v0.0.0-20240408161823-9ad904a10d6d // indirect
	github.com/xuri/nfp v0.0.0-20240318013403-ab9948c2c4a7 // indirect
	golang.org/x/crypto v0.31.0 // indirect
	golang.org/x/net v0.30.0 // indirect
	golang.org/x/text v0.21.0 // indirect
)
