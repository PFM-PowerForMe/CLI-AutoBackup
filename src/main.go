package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

// ==========================================
// 1. 全局配置与日志缓冲
// ==========================================

type Config struct {
	BackupName       string
	BackupPath       string
	EncryptionPubKey string
	OSSAccessKeyID   string
	OSSAccessKeySec  string
	OSSBucket        string
	OSSRegion        string // 改为 Region，由存储后端自动拼接
	PushURL          string
	PushToken        string
	DaysToKeep       int
}

type MemLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *MemLogger) Write(p []byte) (n int, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	msg := string(p)
	l.lines = append(l.lines, msg)
	os.Stdout.Write(p)
	return len(p), nil
}

func (l *MemLogger) GetLogs() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "")
}

var memLogger = &MemLogger{}

func init() {
	log.SetOutput(memLogger)
	log.SetFlags(log.Ldate | log.Ltime)
}

// ==========================================
// 2. 存储抽象层 (方便未来接入 S3, WebDAV 等)
// ==========================================

// StorageProvider 定义了所有存储后端必须实现的方法
type StorageProvider interface {
	Upload(localFile, remoteKey string) error
	CleanOldBackups(backupName string, daysToKeep int)
}

// ==========================================
// 3. 阿里云 OSS 实现
// ==========================================

type AliyunOSSProvider struct {
	Endpoint        string
	AccessKeyID     string
	AccessKeySecret string
	BucketName      string
}

// NewAliyunOSSProvider 实例化阿里云 OSS，并处理 Endpoint 自动拼接
func NewAliyunOSSProvider(region, ak, sk, bucket string) (*AliyunOSSProvider, error) {
	endpoint := strings.TrimSpace(region)
	
	// 智能拼接逻辑：
	// 如果用户填的是 cn-hongkong，没有包含 "."，则自动组装成公网域名
	if !strings.Contains(endpoint, ".") && !strings.HasPrefix(endpoint, "http") {
		endpoint = fmt.Sprintf("oss-%s.aliyuncs.com", endpoint)
	}
	
	// 强制默认使用 HTTPS 提升安全性
	if !strings.HasPrefix(endpoint, "http") {
		endpoint = "https://" + endpoint
	}

	return &AliyunOSSProvider{
		Endpoint:        endpoint,
		AccessKeyID:     ak,
		AccessKeySecret: sk,
		BucketName:      bucket,
	}, nil
}

func (p *AliyunOSSProvider) Upload(localFile, remoteKey string) error {
	client, err := oss.New(p.Endpoint, p.AccessKeyID, p.AccessKeySecret)
	if err != nil {
		return fmt.Errorf("初始化 OSS 客户端失败: %v", err)
	}
	bucket, err := client.Bucket(p.BucketName)
	if err != nil {
		return fmt.Errorf("获取 Bucket 失败: %v", err)
	}

	log.Printf("开始上传到 OSS: %s (Endpoint: %s)", remoteKey, p.Endpoint)
	if err = bucket.PutObjectFromFile(remoteKey, localFile); err != nil {
		return err
	}
	log.Printf("OSS 上传成功!")
	return nil
}

func (p *AliyunOSSProvider) CleanOldBackups(backupName string, daysToKeep int) {
	log.Printf("开始清理 OSS 中 %d 天前的旧备份...", daysToKeep)
	client, err := oss.New(p.Endpoint, p.AccessKeyID, p.AccessKeySecret)
	if err != nil {
		log.Printf("获取 OSS Client 失败，跳过清理: %v", err)
		return
	}
	bucket, err := client.Bucket(p.BucketName)
	if err != nil {
		log.Printf("获取 Bucket 失败: %v", err)
		return
	}

	prefix := fmt.Sprintf("file/%s_", backupName)
	marker := oss.Marker("")
	dateRegex := regexp.MustCompile(fmt.Sprintf(`%s_(\d{4}-\d{2}-\d{2})`, regexp.QuoteMeta(backupName)))
	cutoffDate := time.Now().AddDate(0, 0, -daysToKeep)

	for {
		lsRes, err := bucket.ListObjects(oss.Prefix(prefix), marker)
		if err != nil {
			log.Printf("列出 OSS 对象失败: %v", err)
			break
		}
		for _, object := range lsRes.Objects {
			matches := dateRegex.FindStringSubmatch(object.Key)
			if len(matches) > 1 {
				if fileDate, err := time.Parse("2006-01-02", matches[1]); err == nil && fileDate.Before(cutoffDate) {
					log.Printf("正在删除旧备份: %s", object.Key)
					bucket.DeleteObject(object.Key)
				}
			}
		}
		if lsRes.IsTruncated {
			marker = oss.Marker(lsRes.NextMarker)
		} else {
			break
		}
	}
	log.Printf("旧备份清理完成。")
}

// ==========================================
// 4. 核心业务逻辑 (配置、打包、加密、通知)
// ==========================================

func printHelp() {
	colorReset := "\033[0m"
	colorGreen := "\033[32m"
	colorRed := "\033[31m"

	vars := []struct {
		Name     string
		Desc     string
		Required bool
		Default  string
		IsSecret bool
	}{
		{"CR_AUTOBACKUP_BACKUP_NAME", "备份任务的名称前缀", false, "app-backup", false},
		{"CR_AUTOBACKUP_BACKUP_PATH", "需要打包备份的本地目录路径", true, "", false},
		{"CR_AUTOBACKUP_ENCRYPTION_PUB_KEY", "Base64编码的GPG公钥 (ASCII Armored)", true, "", false},
		{"CR_AUTOBACKUP_OSS_ACCESS_KEY_ID", "阿里云OSS AccessKey ID", true, "", false},
		{"CR_AUTOBACKUP_OSS_ACCESS_KEY_SECRET", "阿里云OSS AccessKey Secret", true, "", true},
		{"CR_AUTOBACKUP_OSS_BUCKET", "阿里云OSS Bucket名称", true, "", false},
		{"CR_AUTOBACKUP_OSS_REGION", "阿里云OSS Region (如: cn-hongkong, 程序会自动拼接)", true, "", false},
		{"CR_AUTOBACKUP_PUSH_URL", "通知推送的Webhook URL", false, "", false},
		{"CR_AUTOBACKUP_PUSH_TOKEN", "通知推送的Token/密钥", false, "", true},
		{"CR_AUTOBACKUP_DAYS_TO_KEEP", "保留旧备份的天数", false, "7", false},
	}

	fmt.Println("\nautobackup - 容器自备份与加密上传工具")
	fmt.Println("==========================================================")

	fmt.Println("\n[ 环境变量说明 ]")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "变量名\t必填\t默认值\t说明")
	fmt.Fprintln(w, "------\t----\t------\t----")
	for _, v := range vars {
		reqStr := "否"
		if v.Required {
			reqStr = "是"
		}
		defStr := v.Default
		if defStr == "" {
			defStr = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.Name, reqStr, defStr, v.Desc)
	}
	w.Flush()

	fmt.Println("\n[ 当前检测到的环境变量 ]")
	for _, v := range vars {
		val := os.Getenv(v.Name)
		if val != "" {
			fmt.Printf("%s: %strue%s\n", v.Name, colorGreen, colorReset)
		} else {
			fmt.Printf("%s: %sfalse%s\n", v.Name, colorRed, colorReset)
		}
	}

	fmt.Println("\n[ 当前环境变量解析 ]")
	for _, v := range vars {
		val := os.Getenv(v.Name)
		if val == "" {
			if v.Default != "" {
				fmt.Printf("%s: 未设置 (将使用默认值: %s)\n", v.Name, v.Default)
			} else {
				fmt.Printf("%s: 未设置\n", v.Name)
			}
			continue
		}

		if v.Name == "CR_AUTOBACKUP_ENCRYPTION_PUB_KEY" {
			decoded, err := base64.StdEncoding.DecodeString(val)
			if err != nil {
				fmt.Printf("%s:\n%s[解析失败] 无法进行 Base64 解码: %v%s\n", v.Name, colorRed, err, colorReset)
			} else {
				fmt.Printf("%s:\n%s%s%s\n", v.Name, colorGreen, string(decoded), colorReset)
			}
		} else if v.IsSecret {
			if len(val) > 4 {
				fmt.Printf("%s: %s***%s\n", v.Name, val[:2], val[len(val)-2:])
			} else {
				fmt.Printf("%s: ***\n", v.Name)
			}
		} else {
			fmt.Printf("%s: %s\n", v.Name, val)
		}
	}
	fmt.Println("==========================================================")
}

func loadConfig() (*Config, error) {
	days, _ := strconv.Atoi(getEnv("CR_AUTOBACKUP_DAYS_TO_KEEP", "7"))
	cfg := &Config{
		BackupName:       getEnv("CR_AUTOBACKUP_BACKUP_NAME", "app-backup"),
		BackupPath:       os.Getenv("CR_AUTOBACKUP_BACKUP_PATH"),
		EncryptionPubKey: os.Getenv("CR_AUTOBACKUP_ENCRYPTION_PUB_KEY"),
		OSSAccessKeyID:   os.Getenv("CR_AUTOBACKUP_OSS_ACCESS_KEY_ID"),
		OSSAccessKeySec:  os.Getenv("CR_AUTOBACKUP_OSS_ACCESS_KEY_SECRET"),
		OSSBucket:        os.Getenv("CR_AUTOBACKUP_OSS_BUCKET"),
		OSSRegion:        os.Getenv("CR_AUTOBACKUP_OSS_REGION"),
		PushURL:          os.Getenv("CR_AUTOBACKUP_PUSH_URL"),
		PushToken:        os.Getenv("CR_AUTOBACKUP_PUSH_TOKEN"),
		DaysToKeep:       days,
	}

	if cfg.BackupPath == "" {
		return nil, fmt.Errorf("环境变量 CR_AUTOBACKUP_BACKUP_PATH 未设置")
	}
	if cfg.EncryptionPubKey == "" {
		return nil, fmt.Errorf("环境变量 CR_AUTOBACKUP_ENCRYPTION_PUB_KEY 未设置")
	}
	if cfg.OSSAccessKeyID == "" || cfg.OSSAccessKeySec == "" || cfg.OSSBucket == "" || cfg.OSSRegion == "" {
		return nil, fmt.Errorf("OSS 相关环境变量 (CR_AUTOBACKUP_OSS_*) 未完全配置")
	}
	return cfg, nil
}

func createEncryptedArchive(sourceDir string, outFile string, pubKeyBase64 string) error {
	decodedKey, err := base64.StdEncoding.DecodeString(pubKeyBase64)
	if err != nil {
		return fmt.Errorf("公钥 Base64 解码失败: %v", err)
	}

	entityList, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(decodedKey))
	if err != nil {
		return fmt.Errorf("解析 GPG 公钥失败: %v", err)
	}
	if len(entityList) == 0 {
		return fmt.Errorf("未找到有效的 GPG 公钥")
	}

	f, err := os.Create(outFile)
	if err != nil {
		return err
	}
	defer f.Close()

	pgpWriter, err := openpgp.Encrypt(f, entityList, nil, nil, nil)
	if err != nil {
		return fmt.Errorf("初始化 GPG 加密流失败: %v", err)
	}
	defer pgpWriter.Close()

	gzWriter := gzip.NewWriter(pgpWriter)
	defer gzWriter.Close()

	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	log.Printf("开始打包并加密目录: %s", sourceDir)
	return filepath.Walk(sourceDir, func(file string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(sourceDir, file)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		header, err := tar.FileInfoHeader(fi, fi.Name())
		if err != nil {
			return err
		}
		header.Name = relPath

		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}

		if !fi.Mode().IsDir() {
			data, err := os.Open(file)
			if err != nil {
				return err
			}
			defer data.Close()
			if _, err := io.Copy(tarWriter, data); err != nil {
				return err
			}
		}
		return nil
	})
}

func pushLogs(cfg *Config) {
	if cfg.PushURL == "" || cfg.PushToken == "" {
		log.Printf("未配置 PushURL 或 PushToken，跳过日志推送。")
		return
	}

	content := fmt.Sprintf("```text\n%s\n```", memLogger.GetLogs())
	data := url.Values{}
	data.Set("title", fmt.Sprintf("%s 备份任务通知", cfg.BackupName))
	data.Set("description", "容器自动备份运行日志")
	data.Set("content", content)
	data.Set("token", cfg.PushToken)

	req, _ := http.NewRequest("POST", cfg.PushURL, bytes.NewBufferString(data.Encode()))
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	if resp, err := client.Do(req); err == nil {
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			log.Printf("日志推送成功！")
			return
		}
	}
	log.Printf("日志推送失败！")
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

// ==========================================
// 5. 主程序入口
// ==========================================

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help") {
		printHelp()
		os.Exit(0)
	}

	cfg, err := loadConfig()
	if err != nil {
		log.Printf("❌ 配置加载失败: %v", err)
		os.Exit(1)
	}

	timestamp := time.Now().Format("2006-01-02")
	tmpFile := filepath.Join(os.TempDir(), fmt.Sprintf("%s_%s.tar.gz.gpg", cfg.BackupName, timestamp))
	remoteKey := fmt.Sprintf("file/%s_%s.tar.gz.gpg", cfg.BackupName, timestamp)

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			os.Remove(tmpFile)
			pushLogs(cfg)
		})
	}
	defer cleanup()

	// 1. 打包并加密
	if err = createEncryptedArchive(cfg.BackupPath, tmpFile, cfg.EncryptionPubKey); err != nil {
		log.Printf("❌ 打包或加密失败: %v", err)
		cleanup()
		os.Exit(1)
	}

	// 2. 初始化存储提供者 (解耦设计，目前注入 AliyunOSS)
	var storage StorageProvider
	storage, err = NewAliyunOSSProvider(cfg.OSSRegion, cfg.OSSAccessKeyID, cfg.OSSAccessKeySec, cfg.OSSBucket)
	if err != nil {
		log.Printf("❌ 存储引擎初始化失败: %v", err)
		cleanup()
		os.Exit(1)
	}

	// 3. 上传文件
	if err = storage.Upload(tmpFile, remoteKey); err != nil {
		log.Printf("❌ 上传失败: %v", err)
		cleanup()
		os.Exit(1)
	}

	// 4. 清理旧备份
	storage.CleanOldBackups(cfg.BackupName, cfg.DaysToKeep)

	log.Printf("✅ 备份任务全部执行成功！")
}
