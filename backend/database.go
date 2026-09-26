package main

import (
	"errors"
	"strings"

	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// Use the driver's parameter encoder to avoid prepare/execute/close round trips
// for one-shot statements. Values remain bound parameters at the GORM boundary;
// never construct SQL by concatenating application input. Explicit DSN settings
// are preserved, including interpolateParams=false for compatibility.
func openDatabase(dsn string, config *gorm.Config) (*gorm.DB, error) {
	encoded, err := databaseDSN(dsn)
	if err != nil {
		return nil, err
	}
	return gorm.Open(mysql.Open(encoded), config)
}

func databaseDSN(dsn string) (string, error) {
	parsed, err := driver.ParseDSN(dsn)
	if err != nil {
		return "", err
	}
	// The driver rejects unsafe collations, but charset= can independently
	// issue SET NAMES. Only opt in for known UTF-8 encodings, including every
	// fallback charset; leave other deployments on prepared parameters.
	safe := strings.HasPrefix(strings.ToLower(parsed.Collation), "utf8")
	if charsets := parsed.Params["charset"]; charsets != "" {
		for _, charset := range strings.Split(charsets, ",") {
			switch strings.ToLower(strings.TrimSpace(charset)) {
			case "utf8", "utf8mb3", "utf8mb4":
			default:
				safe = false
			}
		}
	}
	if parsed.InterpolateParams && !safe {
		return "", errors.New("interpolateParams requires a supported UTF-8 charset and collation")
	}
	if safe && !strings.Contains(dsn, "interpolateParams=") {
		parsed.InterpolateParams = true
	}
	return parsed.FormatDSN(), nil
}
