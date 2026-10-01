package backup

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Object es un respaldo ya guardado en el almacenamiento.
type Object struct {
	Key          string
	LastModified time.Time
}

// ObjectStore es lo mínimo que hace falta del almacenamiento externo.
// Interfaz para poder probar el programador de respaldos sin red.
type ObjectStore interface {
	Put(ctx context.Context, key string, data []byte) error
	List(ctx context.Context, prefix string) ([]Object, error)
	Delete(ctx context.Context, key string) error
}

// S3Config — cualquier almacenamiento compatible con S3 (Cloudflare R2,
// Backblaze B2, AWS S3...).
type S3Config struct {
	Endpoint        string // ej. <id-de-cuenta>.r2.cloudflarestorage.com
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	Region          string
}

type s3Store struct {
	client *minio.Client
	bucket string
}

// NewS3Store crea el cliente. Siempre usa HTTPS.
func NewS3Store(cfg S3Config) (ObjectStore, error) {
	endpoint := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(cfg.Endpoint, "https://"), "http://"), "/")
	region := cfg.Region
	if region == "" {
		region = "auto"
	}
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: true,
		Region: region,
	})
	if err != nil {
		return nil, fmt.Errorf("configuración del almacenamiento inválida: %w", err)
	}
	return &s3Store{client: client, bucket: cfg.Bucket}, nil
}

func (s *s3Store) Put(ctx context.Context, key string, data []byte) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/octet-stream"})
	return err
}

func (s *s3Store) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		out = append(out, Object{Key: obj.Key, LastModified: obj.LastModified})
	}
	return out, nil
}

func (s *s3Store) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}
