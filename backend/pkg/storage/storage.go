// Package storage 封装 S3 兼容对象存储（本地 RustFS/MinIO，生产 OSS——同一 S3 API，D5 决策）。
// 实例不碰文件字节：上传/下载全部经预签名 URL 直连（§1.1 部署原则）。
package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Client struct {
	mc              *minio.Client
	audioBucket     string
	recordingBucket string
}

// New 按 S3_ENDPOINT（支持带 http(s):// 前缀）创建客户端并确保两个 bucket 存在。
func New(endpoint, accessKey, secretKey, audioBucket, recordingBucket string) (*Client, error) {
	secure := strings.HasPrefix(endpoint, "https://")
	host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")

	mc, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: secure,
	})
	if err != nil {
		return nil, fmt.Errorf("对象存储客户端初始化: %w", err)
	}
	c := &Client{mc: mc, audioBucket: audioBucket, recordingBucket: recordingBucket}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, b := range []string{audioBucket, recordingBucket} {
		exists, err := mc.BucketExists(ctx, b)
		if err != nil {
			return nil, fmt.Errorf("检查 bucket %s: %w", b, err)
		}
		if !exists {
			if err := mc.MakeBucket(ctx, b, minio.MakeBucketOptions{}); err != nil {
				return nil, fmt.Errorf("创建 bucket %s: %w", b, err)
			}
		}
	}
	return c, nil
}

// AudioKey 由 cache_key 派生对象 key（全局去重 → 对象路径即缓存身份）。
func AudioKey(cacheKey string) string { return cacheKey + ".mp3" }

// PresignedPUT 生成上传 URL（客户端携 Azure Key 生成 MP3 后直传，FR-3.12）。
func (c *Client) PresignedPUT(ctx context.Context, cacheKey string, ttl time.Duration) (string, error) {
	u, err := c.mc.PresignedPutObject(ctx, c.audioBucket, AudioKey(cacheKey), ttl)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// PresignedGET 生成下载 URL（/audio/{id}/file 302 目标；10 分钟有效，§8.6）。
// objectKey 为完整对象 key（即 audio_asset.storage_url 的值，已含 .mp3 后缀）。
func (c *Client) PresignedGET(ctx context.Context, objectKey string, ttl time.Duration) (string, error) {
	u, err := c.mc.PresignedGetObject(ctx, c.audioBucket, objectKey, ttl, nil)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

// PresignedRecordingPUT 录音上传预签名（FR-9.7 云存储，与音频同一存储层）。
func (c *Client) PresignedRecordingPUT(ctx context.Context, objectKey string, ttl time.Duration) (string, error) {
	u, err := c.mc.PresignedPutObject(ctx, c.recordingBucket, objectKey, ttl)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}
