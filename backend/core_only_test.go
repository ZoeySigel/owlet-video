package main

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

func TestGCFeedIsOnlyBackend(t *testing.T) {
	for _, engine := range []string{"", "legacy", "gcfeed"} {
		t.Setenv("BACKEND_ENGINE", engine)
		a := &App{core: &coreServices{}}
		w := httptest.NewRecorder()
		a.router().ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/capabilities", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"engine":"gcfeed"`) {
			t.Fatalf("BACKEND_ENGINE=%q: %d %s", engine, w.Code, w.Body.String())
		}
	}
}

func TestRemovedLikesFeedIsRejected(t *testing.T) {
	a := &App{core: &coreServices{}}
	w := httptest.NewRecorder()
	a.router().ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/videos?sort=likes", nil))
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"error":"invalid_sort"`) {
		t.Fatalf("removed feed: %d %s", w.Code, w.Body.String())
	}
}

func TestCoreInitializationIsolation(t *testing.T) {
	dsn := os.Getenv("CORE_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("CORE_TEST_MYSQL_DSN not configured")
	}
	root, err := openDatabase(dsn, &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	rootSQL, _ := root.DB()
	t.Cleanup(func() { rootSQL.Close() })
	name := "owlet_init_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := root.Exec("CREATE DATABASE `" + name + "`").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Exec("DROP DATABASE `" + name + "`") })
	cfg, err := driver.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName, cfg.ParseTime = name, true
	db, err := openDatabase(cfg.FormatDSN(), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { sqlDB.Close() })
	// A populated old database must be refused, even if empty of business rows.
	if err := db.AutoMigrate(&User{}); err != nil {
		t.Fatal(err)
	}
	if err := initializeCoreDatabase(db); err == nil {
		t.Fatal("accepted old schema")
	}
	if db.Migrator().HasTable("account") {
		t.Fatal("modified old database")
	}
	if err := db.Migrator().DropTable(&User{}); err != nil {
		t.Fatal(err)
	}
	if err := initializeCoreDatabase(db); err != nil {
		t.Fatal(err)
	}
	invite := Invite{CodeHash: shaHex("preserved-invite"), ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.Create(&invite).Error; err != nil {
		t.Fatal(err)
	}
	if err := initializeCoreDatabase(db); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&Invite{}).Where("id = ?", invite.ID).Count(&count).Error; err != nil || count != 1 {
		t.Fatal("initialization did not preserve existing data", err)
	}
}
