package main

import (
	driver "github.com/go-sql-driver/mysql"
	"testing"
)

func TestDatabaseParameterEncodingPolicy(t *testing.T) {
	for _, tc := range []struct {
		query             string
		enabled, rejected bool
	}{
		{"", true, false},
		{"charset=utf8mb4", true, false},
		{"charset=utf8mb4,utf8", true, false},
		{"charset=utf8mb4&interpolateParams=false", false, false},
		{"charset=gbk", false, false},
		{"charset=utf8mb4,gbk", false, false},
		{"charset=gbk&interpolateParams=true", false, true},
		{"collation=gbk_chinese_ci", false, false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			dsn, err := databaseDSN("user:password@tcp(127.0.0.1:3306)/test?" + tc.query)
			if tc.rejected {
				if err == nil {
					t.Fatal("unsafe encoding accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := driver.ParseDSN(dsn)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.InterpolateParams != tc.enabled {
				t.Fatalf("interpolation=%v", cfg.InterpolateParams)
			}
		})
	}
}
