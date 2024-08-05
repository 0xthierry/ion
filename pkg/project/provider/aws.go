package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/sst/ion/internal/util"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	ssmTypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

type AwsProvider struct {
	config      aws.Config
	profile     string
	credentials sync.Once
}

func (a *AwsProvider) Env() (map[string]string, error) {
	creds, err := a.config.Credentials.Retrieve(context.Background())
	if err != nil {
		return nil, err
	}
	env := map[string]string{}
	env["SST_AWS_ACCESS_KEY_ID"] = creds.AccessKeyID
	env["SST_AWS_SECRET_ACCESS_KEY"] = creds.SecretAccessKey
	env["SST_AWS_SESSION_TOKEN"] = creds.SessionToken
	env["SST_AWS_REGION"] = a.config.Region
	if a.profile != "" {
		env["AWS_PROFILE"] = a.profile
	}
	return env, nil
}

func (a *AwsProvider) Init(app string, stage string, args map[string]interface{}) error {
	ctx := context.Background()
	if os.Getenv("SST_AWS_NO_PROFILE") != "" {
		delete(args, "profile")
	}
	cfg, err := config.LoadDefaultConfig(
		ctx,
		func(lo *config.LoadOptions) error {
			if profile, ok := args["profile"].(string); ok && profile != "" {
				lo.SharedConfigProfile = profile
			}
			if region, ok := args["region"].(string); ok && region != "" {
				lo.Region = region
				lo.DefaultRegion = "us-east-1"
			}
			return nil
		},
	)
	if err != nil {
		return err
	}
	if assumeRole, ok := args["assumeRole"].(map[string]interface{}); ok {
		stsclient := sts.NewFromConfig(cfg)
		cfg.Credentials = stscreds.NewAssumeRoleProvider(stsclient, assumeRole["roleArn"].(string), func(aro *stscreds.AssumeRoleOptions) {
			if sessionName, ok := assumeRole["sessionName"].(string); ok {
				aro.RoleSessionName = sessionName
			}
		})
	}
	_, err = cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return err
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	slog.Info("aws credentials found", "region", cfg.Region)
	a.config = cfg
	// if profile is set in args it gets saved to the provider and always used for removing resources
	// this isn't ideal because people may use different profile names for the same stage
	// so we wipe it from args and put it in env which is not saved to the state
	if profile, ok := args["profile"].(string); ok && profile != "" {
		a.profile = profile
		delete(args, "profile")
	}
	defaultTags, ok := args["defaultTags"].(map[string]interface{})
	if !ok {
		defaultTags = map[string]interface{}{}
	}
	tags, ok := defaultTags["tags"].(map[string]interface{})
	if !ok {
		tags = map[string]interface{}{}
		defaultTags["tags"] = tags
	}
	tags["sst:app"] = app
	tags["sst:stage"] = stage
	args["defaultTags"] = defaultTags
	if args["region"] == nil {
		args["region"] = cfg.Region
	}
	return nil
}

func (a *AwsProvider) Config() aws.Config {
	return a.config
}

type AwsHome struct {
	provider  *AwsProvider
	bootstrap *AwsBootstrapData
}

func NewAwsHome(provider *AwsProvider) *AwsHome {
	return &AwsHome{
		provider: provider,
	}
}

func (a *AwsHome) pathForData(key, app, stage string) string {
	return path.Join(key, app, fmt.Sprintf("%v.json", stage))
}

func (a *AwsHome) pathForPassphrase(app string, stage string) string {
	return "/" + strings.Join([]string{"sst", "passphrase", app, stage}, "/")
}

func (a *AwsHome) getData(key, app, stage string) (io.Reader, error) {
	s3Client := s3.NewFromConfig(a.provider.config)

	result, err := s3Client.GetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(a.bootstrap.State),
		Key:    aws.String(a.pathForData(key, app, stage)),
	})
	if err != nil {
		var nsk *s3types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, nil
		}
		return nil, err
	}
	return result.Body, nil
}

func (a *AwsHome) putData(key, app, stage string, data io.Reader) error {
	s3Client := s3.NewFromConfig(a.provider.config)

	_, err := s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket:      aws.String(a.bootstrap.State),
		Key:         aws.String(a.pathForData(key, app, stage)),
		Body:        data,
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return err
	}

	return nil
}

func (a *AwsHome) removeData(key, app, stage string) error {
	s3Client := s3.NewFromConfig(a.provider.config)

	_, err := s3Client.DeleteObject(context.TODO(), &s3.DeleteObjectInput{
		Bucket: aws.String(a.bootstrap.State),
		Key:    aws.String(a.pathForData(key, app, stage)),
	})
	if err != nil {
		return err
	}

	return nil
}

func (a *AwsHome) getPassphrase(app string, stage string) (string, error) {
	ssmClient := ssm.NewFromConfig(a.provider.config)

	result, err := ssmClient.GetParameter(context.TODO(), &ssm.GetParameterInput{
		Name:           aws.String(a.pathForPassphrase(app, stage)),
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		pnf := &ssmTypes.ParameterNotFound{}
		if errors.As(err, &pnf) {
			return "", nil
		}

		return "", err
	}
	return *result.Parameter.Value, nil
}

func (a *AwsHome) setPassphrase(app, stage, passphrase string) error {
	ssmClient := ssm.NewFromConfig(a.provider.config)

	_, err := ssmClient.PutParameter(context.TODO(), &ssm.PutParameterInput{
		Name:        aws.String(a.pathForPassphrase(app, stage)),
		Type:        ssmTypes.ParameterTypeSecureString,
		Value:       aws.String(passphrase),
		Description: aws.String("DO NOT DELETE STATE WILL BECOME UNRECOVERABLE"),
		Overwrite:   aws.Bool(false),
	})
	return err
}

func (a *AwsHome) Bootstrap() error {
	data, err := AwsBootstrap(a.provider.config)
	if err != nil {
		return err
	}
	a.bootstrap = data
	return err
}

func AwsBootstrap(cfg aws.Config) (*AwsBootstrapData, error) {
	ctx := context.TODO()
	ssmClient := ssm.NewFromConfig(cfg)
	bootstrapData := &AwsBootstrapData{}
	slog.Info("fetching bootstrap")
	result, err := ssmClient.GetParameter(ctx, &ssm.GetParameterInput{
		Name:           aws.String(SSM_NAME_BOOTSTRAP),
		WithDecryption: aws.Bool(false),
	})
	if result != nil && result.Parameter.Value != nil {
		slog.Info("found existing bootstrap", "data", *result.Parameter.Value)
		err = json.Unmarshal([]byte(*result.Parameter.Value), bootstrapData)
		if err != nil {
			return nil, err
		}
	}
	if err != nil {
		var pnf *ssmTypes.ParameterNotFound
		if !errors.As(err, &pnf) {
			return nil, err
		}
	}

	if len(steps) > bootstrapData.Version {
		for index, step := range steps {
			if bootstrapData != nil && bootstrapData.Version > index {
				continue
			}
			slog.Info("running bootstrap step", "step", index)
			err = step(ctx, cfg, bootstrapData)
			if err != nil {
				return nil, err
			}
		}
		bootstrapData.Version = len(steps)
		data, err := json.Marshal(bootstrapData)
		_, err = ssmClient.PutParameter(
			ctx,
			&ssm.PutParameterInput{
				Name:      aws.String(SSM_NAME_BOOTSTRAP),
				Type:      ssmTypes.ParameterTypeString,
				Overwrite: aws.Bool(true),
				Value:     aws.String(string(data)),
			},
		)
		if err != nil {
			return nil, err
		}
	}
	return bootstrapData, nil
}

type AwsBootstrapData struct {
	Version int    `json:"version"`
	Asset   string `json:"asset"`
	State   string `json:"state"`
}

type bootstrapStep = func(ctx context.Context, cfg aws.Config, data *AwsBootstrapData) error

// never change these, only append more steps
var steps = []bootstrapStep{
	func(ctx context.Context, cfg aws.Config, data *AwsBootstrapData) error {
		region := cfg.Region
		rand := util.RandomString(12)
		assetName := fmt.Sprintf("sst-asset-%v", rand)
		stateName := fmt.Sprintf("sst-state-%v", rand)
		slog.Info("creating bootstrap bucket", "name", assetName)
		s3Client := s3.NewFromConfig(cfg)

		var config *s3types.CreateBucketConfiguration = nil
		if region != "us-east-1" {
			config = &s3types.CreateBucketConfiguration{
				LocationConstraint: s3types.BucketLocationConstraint(region),
			}
		}
		_, err := s3Client.CreateBucket(context.TODO(), &s3.CreateBucketInput{
			Bucket:                    aws.String(assetName),
			CreateBucketConfiguration: config,
		})
		if err != nil {
			return err
		}

		_, err = s3Client.PutBucketNotificationConfiguration(context.TODO(), &s3.PutBucketNotificationConfigurationInput{
			Bucket:                    aws.String(assetName),
			NotificationConfiguration: &s3types.NotificationConfiguration{},
		})
		if err != nil {
			return err
		}

		_, err = s3Client.CreateBucket(context.TODO(), &s3.CreateBucketInput{
			Bucket:                    aws.String(stateName),
			CreateBucketConfiguration: config,
		})
		if err != nil {
			return err
		}

		_, err = s3Client.PutBucketVersioning(context.TODO(), &s3.PutBucketVersioningInput{
			Bucket: aws.String(stateName),
			VersioningConfiguration: &s3types.VersioningConfiguration{
				Status: s3types.BucketVersioningStatusEnabled,
			},
		})
		if err != nil {
			return err
		}
		data.Asset = assetName
		data.State = stateName
		return nil
	},
}
