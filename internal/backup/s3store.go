package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type s3Store struct {
	client *s3.Client
	bucket string
}

func newS3Store(ctx context.Context, cfg S3Config) (*s3Store, error) {
	region := cfg.Region
	if region == "" {
		region = "auto" // Cloudflare R2 requires "auto"; harmless for AWS S3
	}
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			cfg.AccessKeyID, cfg.SecretKey, "",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("load S3 config: %w", err)
	}
	opts := []func(*s3.Options){
		func(o *s3.Options) {
			o.UsePathStyle = cfg.ForcePathStyle
		},
	}
	if cfg.Endpoint != "" {
		opts = append(opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		})
	}
	return &s3Store{
		client: s3.NewFromConfig(awsCfg, opts...),
		bucket: cfg.Bucket,
	}, nil
}

func (s *s3Store) upload(ctx context.Context, key string, body io.Reader) (int64, error) {
	data, err := io.ReadAll(body)
	if err != nil {
		return 0, fmt.Errorf("read body: %w", err)
	}
	ct := "application/gzip"
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &key,
		Body:        bytes.NewReader(data),
		ContentType: &ct,
	})
	if err != nil {
		return 0, fmt.Errorf("S3 PutObject: %w", err)
	}
	return int64(len(data)), nil
}

func (s *s3Store) delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	})
	return err
}

func (s *s3Store) presignURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	pc := s3.NewPresignClient(s.client)
	disp := fmt.Sprintf("attachment; filename=%q", path.Base(key))
	res, err := pc.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     &s.bucket,
		Key:                        &key,
		ResponseContentDisposition: &disp,
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", fmt.Errorf("presign: %w", err)
	}
	return res.URL, nil
}

func (s *s3Store) headBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket})
	if err != nil {
		return fmt.Errorf("S3 HeadBucket: %w", err)
	}
	return nil
}
