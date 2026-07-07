// setup-r2 configura as lifecycle rules do bucket R2, garantindo que tanto
// videos/ quanto previews/ expirem automaticamente após 24 h.
//
// Uso:
//
//	go run ./cmd/setup-r2/
//
// Variáveis de ambiente (ou .env na raiz do projeto):
//
//	REPLAY_R2_ACCOUNT_ID
//	REPLAY_R2_ACCESS_KEY_ID
//	REPLAY_R2_SECRET_ACCESS_KEY
//	REPLAY_R2_BUCKET
package main

import (
	"context"
	"fmt"
	"log"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/joho/godotenv"

	"github.com/edipo/replay-saas/internal/envutil"
)

func main() {
	_ = godotenv.Load()

	accountID := envutil.MustEnv("REPLAY_R2_ACCOUNT_ID")
	accessKey := envutil.MustEnv("REPLAY_R2_ACCESS_KEY_ID")
	secretKey := envutil.MustEnv("REPLAY_R2_SECRET_ACCESS_KEY")
	bucket    := envutil.MustEnv("REPLAY_R2_BUCKET")

	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID)
	s3client := s3.New(s3.Options{
		BaseEndpoint: aws.String(endpoint),
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		Region:       "auto",
		UsePathStyle: true,
	})

	ctx := context.Background()

	rules := []types.LifecycleRule{
		{
			ID:     aws.String("expire-videos-24h"),
			Status: types.ExpirationStatusEnabled,
			Filter: &types.LifecycleRuleFilterMemberPrefix{Value: "videos/"},
			Expiration: &types.LifecycleExpiration{
				Days: aws.Int32(1),
			},
		},
		{
			ID:     aws.String("expire-previews-24h"),
			Status: types.ExpirationStatusEnabled,
			Filter: &types.LifecycleRuleFilterMemberPrefix{Value: "previews/"},
			Expiration: &types.LifecycleExpiration{
				Days: aws.Int32(1),
			},
		},
	}

	_, err := s3client.PutBucketLifecycleConfiguration(ctx, &s3.PutBucketLifecycleConfigurationInput{
		Bucket: aws.String(bucket),
		LifecycleConfiguration: &types.BucketLifecycleConfiguration{
			Rules: rules,
		},
	})
	if err != nil {
		log.Fatalf("put lifecycle: %v", err)
	}

	fmt.Printf("Lifecycle rules aplicadas no bucket %q:\n", bucket)
	for _, r := range rules {
		p := r.Filter.(*types.LifecycleRuleFilterMemberPrefix).Value
		fmt.Printf("  %-12s → expira em %d dia(s)\n", p, *r.Expiration.Days)
	}
}

