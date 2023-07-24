package poe_api_go

import (
	"embed"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
)

const (
	gqlURL      = "https://poe.com/api/gql_POST"
	gqlRecvURL  = "https://poe.com/api/receive_POST"
	homeURL     = "https://poe.com"
	settingsURL = "https://poe.com/api/settings"
)

//go:embed poe_graphql/*.graphql
var graphql embed.FS
var queries = make(map[string]string)

var logger = log.New(os.Stdout, "", log.Ldate|log.Ltime|log.Lshortfile)

var userAgent = "This will be ignored! See the README for info on how to set custom headers."
var headers = fhttp.Header{
	"User-Agent": []string{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/114.0.0.0 Safari/537.36 Edg/114.0.1823.82"},
	"Accept":     []string{"text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"},
	// "Accept-Encoding":           []string{"gzip, deflate, br"},
	// "Accept-Language":           []string{"en-US,en;q=0.5"},
	// "Te":                        []string{"trailers"},
	// "Upgrade-Insecure-Requests": []string{"1"},
}

func init() {
	loadQueries()
}

func loadQueries() {
	queryFS, err := fs.Sub(graphql, "poe_graphql")
	if err != nil {
		panic(err)
	}
	// 遍历嵌入的查询文件
	err = fs.WalkDir(queryFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".graphql" {
			return nil
		}

		queryBytes, err := fs.ReadFile(queryFS, path)
		if err != nil {
			return err
		}

		// 将查询文件内容存储到 queries 映射中
		queries[strings.TrimSuffix(d.Name(), filepath.Ext(d.Name()))] = string(queryBytes)
		return nil
	})
	if err != nil {
		panic(err)
	}
}
