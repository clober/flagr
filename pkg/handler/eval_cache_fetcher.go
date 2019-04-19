package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/checkr/flagr/pkg/config"
	"github.com/checkr/flagr/pkg/entity"
	"github.com/checkr/flagr/pkg/util"
	"github.com/jinzhu/gorm"
)

// EvalCacheJSON is the JSON serialization format of EvalCache's flags
type EvalCacheJSON struct {
	Flags []entity.Flag
}

func (ecj *EvalCacheJSON) read(r io.ReadCloser) error {
	defer r.Close()

	b, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}
	err = json.Unmarshal(b, ecj)
	if err != nil {
		return err
	}
	return nil
}

func (ec *EvalCache) export() EvalCacheJSON {
	fs := make([]entity.Flag, 0, len(ec.idCache))

	ec.mapCacheLock.RLock()
	defer ec.mapCacheLock.RUnlock()

	for _, f := range ec.idCache {
		ff := *f
		fs = append(fs, ff)
	}
	return EvalCacheJSON{Flags: fs}
}

func (ec *EvalCache) fetchAllFlags() (idCache mapCache, keyCache mapCache, err error) {
	fs, err := fetchAllFlags()
	if err != nil {
		return nil, nil, err
	}

	idCache = make(map[string]*entity.Flag)
	keyCache = make(map[string]*entity.Flag)

	for i := range fs {
		f := &fs[i]
		if err := f.PrepareEvaluation(); err != nil {
			return nil, nil, err
		}

		if f.ID != 0 {
			idCache[util.SafeString(f.ID)] = f
		}
		if f.Key != "" {
			keyCache[f.Key] = f
		}
	}
	return idCache, keyCache, nil
}

type evalCacheFetcher interface {
	fetch() ([]entity.Flag, error)
}

func newFetcher() (evalCacheFetcher, error) {
	if !config.Config.EvalOnlyMode {
		return &dbFetcher{db: getDB()}, nil
	}

	switch config.Config.DBDriver {
	case "json_file":
		return &jsonFileFetcher{filePath: config.Config.DBConnectionStr}, nil
	case "json_http":
		return &jsonHTTPFetcher{url: config.Config.DBConnectionStr}, nil
	default:
		return nil, fmt.Errorf(
			"failed to create evaluation cache fetcher. DBDriver:%s is not supported",
			config.Config.DBDriver,
		)
	}
}

var fetchAllFlags = func() ([]entity.Flag, error) {
	fetcher, err := newFetcher()
	if err != nil {
		return nil, err
	}
	return fetcher.fetch()
}

type jsonFileFetcher struct {
	filePath string
}

func (ff *jsonFileFetcher) fetch() ([]entity.Flag, error) {
	file, err := os.Open(ff.filePath)
	if err != nil {
		return nil, err
	}
	ecj := &EvalCacheJSON{}
	err = ecj.read(file)
	if err != nil {
		return nil, err
	}
	return ecj.Flags, nil
}

type jsonHTTPFetcher struct {
	url string
}

func (hf *jsonHTTPFetcher) fetch() ([]entity.Flag, error) {
	client := http.Client{Timeout: config.Config.EvalCacheRefreshTimeout}
	res, err := client.Get(hf.url)
	if err != nil {
		return nil, err
	}

	ecj := &EvalCacheJSON{}
	err = ecj.read(res.Body)
	if err != nil {
		return nil, err
	}
	return ecj.Flags, nil
}

type dbFetcher struct {
	db *gorm.DB
}

func (df *dbFetcher) fetch() ([]entity.Flag, error) {
	// Use eager loading to avoid N+1 problem
	// doc: http://jinzhu.me/gorm/crud.html#preloading-eager-loading
	fs := []entity.Flag{}
	err := entity.PreloadSegmentsVariants(df.db).Find(&fs).Error
	return fs, err
}

type s3Fetcher struct {
	region  string
	bucket  string
	key     string
	retries int
	timeout time.Duration
	session *session.Session
}

func newS3Fetcher(connectionStr string) (*s3Fetcher, error) {
	keyValus := strings.Split(connectionStr, " ")

	se, err := session.NewSession(&aws.Config{
		CredentialsChainVerboseErrors: nil,
		Credentials:                   &credentials.Credentials{},
		Endpoint:                      nil,
		Region:                        nil,
		MaxRetries:                    nil,
		S3ForcePathStyle:              util.BoolPtr(true),
	})
	return &s3Fetcher{
		session: se,
	}, nil
}

func (sf *s3Fetcher) fetch() ([]entity.Flag, error) {
	svc := s3.New(sf.session)
	req := &s3.GetObjectInput{
		Bucket: util.StringPtr(sf.bucket),
		Key:    util.StringPtr(sf.key),
	}
	res, err := svc.GetObject(req)
	if err != nil {
		return nil, err
	}

	ecj := &EvalCacheJSON{}
	err = ecj.read(res.Body)
	if err != nil {
		return nil, err
	}
	return ecj.Flags, nil
}