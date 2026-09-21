// Package config 保存全部持久化状态。
//
// 两个上游的账号放在同一份配置里，但按 provider 字段区分归属 ——
// 网关只认「账号属于哪个上游」，不关心它的协议细节。
//
// 设计取舍：全部用 JSON 文件 + 原子写，不引入数据库。
// 理由：数据量小（账号 + 密钥 + 设置），全 JSON 意味着用户免 SSH 即可
// 备份与迁移（拷贝 var/data 目录即可），这是上游 agnes-hub-go 的做法，值得沿用。
package config

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Account 是一个上游账号。
//
// 两个上游共用这一个结构：它们的差异（Agnes 是 Bearer API Key，
// Copilot 是 OAuth 令牌 + 设备指纹）用 Auth 字段承载，而不是拆成两个类型 ——
// 账号池、限流、熔断这些逻辑要能一视同仁地处理它们。
type Account struct {
	// ID 稳定不变，用于日志与任务映射。生成后不要改。
	ID string `json:"id"`
	// Provider 归属上游，例如 "agnes" / "copilot"。
	Provider string `json:"provider"`
	// Name 是用户可读的显示名，允许中文。
	Name string `json:"name"`
	// BaseURL 上游站点根（Agnes 用；Copilot 忽略）。
	BaseURL string `json:"base_url,omitempty"`
	// APIKey 上游凭据（Agnes 用；Copilot 忽略）。
	APIKey string `json:"api_key,omitempty"`
	// Auth 承载非 Bearer 的凭据（Copilot 的令牌等），结构由各 provider 自行解释。
	Auth map[string]string `json:"auth,omitempty"`
	// Models 是该账号声明的可用模型（上游支持账号级声明）。
	Models []string `json:"models,omitempty"`
	// Enabled 为 false 时该账号不参与调度。
	Enabled bool `json:"enabled"`
	// RPM 是该账号的每分钟请求上限。0 表示用全局默认。
	RPM int `json:"rpm,omitempty"`
	// CreatedAt 便于排查「这个账号是什么时候加的」。
	CreatedAt time.Time `json:"created_at"`
}

// Settings 是全局设置。
type Settings struct {
	// ListenAddr 为空时由命令行决定。
	ListenAddr string `json:"listen_addr,omitempty"`
	// DefaultRPM 是未单独指定 RPM 的账号的默认值。
	DefaultRPM int `json:"default_rpm"`
	// MaxRetries 是单次请求最多换几个账号重试。
	MaxRetries int `json:"max_retries"`
	// BackoffBaseMS 是换账号重试的退避基数（毫秒）。
	BackoffBaseMS int `json:"backoff_base_ms"`
	// ChatTimeoutSec 是单次对话请求的超时（秒）。
	//
	// 必须有这个上限。没有它，上游一旦不响应，请求会一直挂着 ——
	// 用户看到的是「转了十几分钟然后 504」，而那个 504 还是反代给的，
	// 网关自己毫无察觉。有超时才能快速失败、换账号重试。
	ChatTimeoutSec int `json:"chat_timeout_sec"`
	// ImageTimeoutSec 是单次生图请求的超时（秒）。
	//
	// 比对话长得多：生图本来就慢，上游要渲染、审核、上传。
	ImageTimeoutSec int `json:"image_timeout_sec"`
}

// DefaultSettings 是首次运行时的设置。
//
// DefaultRPM 取 10：上游 Agnes 官方限流较紧，宁可保守。
// 用户可在控制台按账号覆盖。
func DefaultSettings() Settings {
	return Settings{
		DefaultRPM: 10, MaxRetries: 3, BackoffBaseMS: 400,
		// 对话 120 秒：普通对话远超不了这个数，超过就说明卡住了。
		ChatTimeoutSec: 120,
		// 生图 300 秒：上游渲染 + 审核 + 上传，给足时间。
		ImageTimeoutSec: 300,
	}
}

// Store 是配置的读写入口。所有方法可并发调用。
type Store struct {
	mu   sync.RWMutex
	path string
	// AccountList 是账号集合。字段名不叫 Accounts，因为 Accounts() 是它的读方法。
	AccountList []Account `json:"accounts"`
	// APIKeys 是下游密钥（只存散列）。字段名不用 Keys，
	// 因为 Keys() 是它的读方法，同名会造成混淆。
	APIKeys  []Key    `json:"keys,omitempty"`
	Settings Settings `json:"settings"`
	// Admin 是管理端口令的散列。为 nil 表示尚未设置 ——
	// 首次部署时控制台会引导设置，设置前只允许本机访问。
	Admin *AdminAuth `json:"admin,omitempty"`
}

// ErrNotFound 表示目标不存在。
var ErrNotFound = errors.New("not found")

// NewStore 打开（或初始化）数据目录下的配置。
func NewStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败：%w", err)
	}
	s := &Store{
		path:     filepath.Join(dataDir, "config.json"),
		Settings: DefaultSettings(),
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		// 首次运行：写一份带默认值的空配置，方便用户直接改。
		if err := s.save(); err != nil {
			return nil, err
		}
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取配置失败：%w", err)
	}
	if err := json.Unmarshal(data, s); err != nil {
		return nil, fmt.Errorf("解析配置失败：%w", err)
	}
	// 老配置文件可能缺字段，用默认值补齐。
	if s.Settings.DefaultRPM <= 0 {
		s.Settings.DefaultRPM = DefaultSettings().DefaultRPM
	}
	if s.Settings.MaxRetries <= 0 {
		s.Settings.MaxRetries = DefaultSettings().MaxRetries
	}
	if s.Settings.BackoffBaseMS <= 0 {
		s.Settings.BackoffBaseMS = DefaultSettings().BackoffBaseMS
	}
	return s, nil
}

// Path 返回配置文件路径，供控制台显示。
func (s *Store) Path() string { return s.path }

// Accounts 返回全部账号（含停用的），按 ID 稳定排序。
//
// 与 AccountsFor 的区别：那个只给调度用（只取启用的），
// 这个给控制台用 —— 用户需要看到自己停用了哪些账号。
func (s *Store) Accounts() []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Account, len(s.AccountList))
	copy(out, s.AccountList)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AccountsFor 返回某个上游下所有启用的账号，顺序稳定。
//
// 稳定排序很重要：账号池的粘性与轮询都建立在「同一份配置每次得到同样的顺序」
// 之上，否则重启后调度行为会漂移。
func (s *Store) AccountsFor(providerName string) []Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Account, 0, len(s.AccountList))
	for _, a := range s.AccountList {
		if a.Provider == providerName && a.Enabled {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RPMFor 返回账号的生效 RPM。
func (s *Store) RPMFor(a Account) int {
	if a.RPM > 0 {
		return a.RPM
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.Settings.DefaultRPM > 0 {
		return s.Settings.DefaultRPM
	}
	return DefaultSettings().DefaultRPM
}

// UpsertAccount 新增或更新账号。ID 为空时自动生成。
func (s *Store) UpsertAccount(a Account) (Account, error) {
	s.mu.Lock()
	if a.ID == "" {
		id, err := newID()
		if err != nil {
			s.mu.Unlock()
			return Account{}, err
		}
		a.ID = id
		a.CreatedAt = time.Now().UTC()
	}
	if a.Provider == "" {
		s.mu.Unlock()
		return Account{}, errors.New("账号缺少 provider")
	}
	replaced := false
	for i := range s.AccountList {
		if s.AccountList[i].ID == a.ID {
			s.AccountList[i] = a
			replaced = true
			break
		}
	}
	if !replaced {
		s.AccountList = append(s.AccountList, a)
	}
	s.mu.Unlock()
	if err := s.save(); err != nil {
		return Account{}, err
	}
	return a, nil
}

// DeleteAccount 删除账号。不存在时返回 ErrNotFound。
func (s *Store) DeleteAccount(id string) error {
	s.mu.Lock()
	idx := -1
	for i := range s.AccountList {
		if s.AccountList[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		s.mu.Unlock()
		return ErrNotFound
	}
	s.AccountList = append(s.AccountList[:idx], s.AccountList[idx+1:]...)
	s.mu.Unlock()
	return s.save()
}

// UpdateSettings 覆盖全局设置。
func (s *Store) UpdateSettings(v Settings) error {
	s.mu.Lock()
	s.Settings = v
	s.mu.Unlock()
	return s.save()
}

// save 原子写：先写临时文件再 rename，避免进程被杀时留下半截配置。
func (s *Store) save() error {
	s.mu.RLock()
	data, err := json.MarshalIndent(s, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写临时配置失败：%w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("替换配置失败：%w", err)
	}
	return nil
}

// newID 生成 16 字节随机 ID。
//
// 刻意不用 github.com/google/uuid：标准库 crypto/rand 足够，
// 少一个依赖就少一份供应链风险（M365 那边为此多引了一个包）。
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// 版本位与变体位按 RFC 4122 设置，让它长得像标准 UUID，
	// 便于用户复制到别处使用。
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var sb strings.Builder
	for i, x := range b {
		if i == 4 || i == 6 || i == 8 || i == 10 {
			sb.WriteByte('-')
		}
		fmt.Fprintf(&sb, "%02x", x)
	}
	return sb.String(), nil
}
