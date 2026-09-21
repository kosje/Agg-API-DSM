module aggapi

go 1.22

// 已去掉的依赖及替代方案：
//
//   github.com/google/uuid      -> crypto/rand 自己拼 16 字节，15 处引用全可替换
//   github.com/ledongthuc/pdf   -> 仅 1 处（chat_docs.go 的附件解析），首版不做附件
//   github.com/xuri/excelize    -> 同上，仅 1 处
//   github.com/tiktoken-go      -> 仅 1 处（codex_usage.go 的 token 估算），改用字符数近似
//   golang.org/x/net/proxy      -> 仅 1 处（outbound/proxy.go 的 SOCKS），首版只支持 HTTP 代理
