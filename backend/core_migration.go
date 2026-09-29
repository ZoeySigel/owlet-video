package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	embedding "github.com/ZoeySigel/owlet-video/backend/internal/application/embedding"
	videoapp "github.com/ZoeySigel/owlet-video/backend/internal/application/video"
	account "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/account"
	embedrepo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/embedding"
	interaction "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/interaction"
	message "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/message"
	migration "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/migration"
	relation "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/relation"
	video "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/video"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

const coreBaseline = "8cf995cfa03985c335594c453ea050df5c0f1b6a"

type CoreMigration struct {
	ID        string `gorm:"primaryKey;size:64"`
	Source    string `gorm:"size:64"`
	State     string `gorm:"size:20"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Views preserve the extension read contracts without maintaining two copies of
// users, videos or interactions. Only the account and notification views are
// used for simple extension updates. Core writes always use core repositories.
var coreViews = []string{
	`CREATE OR REPLACE VIEW users AS SELECT id, account AS username, password AS password_hash, bio, avatar_url, created_at FROM account`,
	`CREATE OR REPLACE VIEW videos AS SELECT v.id, v.author_id AS user_id, v.title, v.description, v.file_path, v.size, v.cover_path, COALESCE(s.like_count,0) AS likes_count, COALESCE(s.comment_count,0) AS comments_count, COALESCE(s.favorite_count,0) AS favorites_count, COALESCE(s.like_count,0)*3+COALESCE(s.comment_count,0)*5+COALESCE(s.favorite_count,0)*4 AS popularity, v.published_at, v.created_at FROM video v LEFT JOIN video_stat s ON s.video_id=v.id WHERE v.status=2`,
	`CREATE OR REPLACE VIEW likes AS SELECT id,user_id,video_id,created_at FROM interaction_action WHERE action_type='like' AND status=1`,
	`CREATE OR REPLACE VIEW comments AS SELECT id,user_id,video_id,content AS body,created_at FROM interaction_comment WHERE status=1`,
	`CREATE OR REPLACE VIEW follows AS SELECT id,user_id AS follower_id,target_user_id AS following_id,created_at FROM user_follow WHERE status=1`,
	`CREATE OR REPLACE VIEW notifications AS SELECT id,user_id,actor_id,video_id,type AS kind,content AS body,read_at,created_at FROM user_message`,
}

func prepareCoreSchema(db *gorm.DB) error {
	// Never AutoMigrate the legacy models over these views.
	if err := migration.AutoMigrate(db); err != nil {
		return err
	}
	return db.AutoMigrate(&CoreMigration{}, &Invite{}, &Session{}, &Upload{}, &VideoTag{}, &Message{}, &Outbox{}, &ProcessedEvent{}, &FeedSnapshot{}, &InteractionCommand{})
}

func installCoreViews(db *gorm.DB) error {
	for _, stmt := range coreViews {
		if err := db.Exec(stmt).Error; err != nil {
			return err
		}
	}
	return nil
}

func coreReady(db *gorm.DB) error {
	var m CoreMigration
	if err := db.First(&m, "id = ? AND state = ?", coreBaseline, "verified").Error; err != nil {
		return errors.New("GCFeed database is not verified; run migrate-core against a separate target database first")
	}
	return nil
}

func distinctMigrationDSNs(source, target string) error {
	s, err := driver.ParseDSN(source)
	if err != nil {
		return errors.New("invalid source DSN")
	}
	t, err := driver.ParseDSN(target)
	if err != nil {
		return errors.New("invalid target DSN")
	}
	if s.DBName == "" || t.DBName == "" || s.DBName == t.DBName {
		return errors.New("source and target must use different non-empty database names")
	}
	return nil
}

func migrateCore(ctx context.Context, apply bool) error {
	sourceDSN, targetDSN := os.Getenv("SOURCE_MYSQL_DSN"), os.Getenv("TARGET_MYSQL_DSN")
	if err := distinctMigrationDSNs(sourceDSN, targetDSN); err != nil {
		return err
	}
	source, err := openDatabase(sourceDSN, &gorm.Config{})
	if err != nil {
		return errors.New("cannot connect to source database")
	}
	sourceSQL, err := source.DB()
	if err != nil {
		return err
	}
	defer sourceSQL.Close()
	target, err := openDatabase(targetDSN, &gorm.Config{})
	if err != nil {
		return errors.New("cannot connect to target database")
	}
	targetSQL, err := target.DB()
	if err != nil {
		return err
	}
	defer targetSQL.Close()
	source = source.WithContext(ctx)
	target = target.WithContext(ctx)
	var pending int64
	if err := source.Model(&Outbox{}).Where("published_at IS NULL").Count(&pending).Error; err != nil {
		return err
	}
	if pending != 0 {
		return fmt.Errorf("source has %d unpublished outbox events; stop writes and drain the old worker", pending)
	}
	if source.Migrator().HasTable(&InteractionCommand{}) {
		if err := source.Model(&InteractionCommand{}).Where("completed_at IS NULL").Count(&pending).Error; err != nil {
			return err
		}
	}
	if pending != 0 {
		return fmt.Errorf("source has %d pending interaction commands", pending)
	}
	if err := validateMigrationMedia(source, env("SOURCE_DATA_DIR", "./data")); err != nil {
		return err
	}
	counts := map[string]int64{}
	for _, table := range []string{"users", "videos", "likes", "comments", "follows", "messages", "notifications", "sessions", "invites", "uploads", "video_tags"} {
		var n int64
		if err := source.Table(table).Count(&n).Error; err != nil {
			return err
		}
		counts[table] = n
	}
	if !apply {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"mode": "inspect", "sourceCounts": counts, "baseline": coreBaseline, "writesPerformed": false})
	}
	if os.Getenv("MIGRATION_SOURCE_QUIESCED") != "1" {
		return errors.New("apply requires MIGRATION_SOURCE_QUIESCED=1 after stopping API writes and draining workers")
	}
	// The target must be empty. A completed run is verified, not replayed over
	// post-cutover writes. Failed transactions can be retried with the marker.
	var existing CoreMigration
	scfg, _ := driver.ParseDSN(sourceDSN)
	if target.Migrator().HasTable(&CoreMigration{}) {
		if err := target.First(&existing, "id = ?", coreBaseline).Error; err != nil {
			return errors.New("target has no matching migration marker")
		} else {
			if existing.Source != scfg.DBName {
				return errors.New("migration source differs from target marker")
			}
			if existing.State == "verified" {
				return errors.New("target is already verified; refusing to overwrite or replay migrated data")
			}
			if existing.State == "copied" {
				if err := copyCoreMedia(os.Getenv("SOURCE_DATA_DIR"), os.Getenv("TARGET_DATA_DIR")); err != nil {
					return err
				}
				return finishCoreMigration(source, target, counts)
			}
			if existing.State != "prepared" {
				return errors.New("unknown migration state")
			}
		}
	} else {
		var tables int64
		if err := target.Raw("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE()").Scan(&tables).Error; err != nil {
			return err
		}
		if tables != 0 {
			return errors.New("target database must be empty")
		}
	}
	if err := copyCoreMedia(os.Getenv("SOURCE_DATA_DIR"), os.Getenv("TARGET_DATA_DIR")); err != nil {
		return err
	}
	if err := prepareCoreSchema(target); err != nil {
		return err
	}
	for _, table := range []string{"account", "video", "interaction_action", "interaction_comment", "user_follow", "user_message", "messages"} {
		var n int64
		if err := target.Table(table).Count(&n).Error; err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("target %s is not empty", table)
		}
	}
	marker := CoreMigration{ID: coreBaseline, Source: scfg.DBName, State: "prepared"}
	if err := target.Save(&marker).Error; err != nil {
		return err
	}
	err = source.Transaction(func(snapshot *gorm.DB) error {
		return target.Transaction(func(tx *gorm.DB) error {
			if err := copyCoreData(snapshot, tx); err != nil {
				return err
			}
			return tx.Model(&CoreMigration{}).Where("id = ?", coreBaseline).Update("state", "copied").Error
		})
	}, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return err
	}
	return finishCoreMigration(source, target, counts)
}

func validateMigrationMedia(db *gorm.DB, root string) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	var rows []Video
	return db.Model(&Video{}).FindInBatches(&rows, 200, func(_ *gorm.DB, _ int) error {
		for _, v := range rows {
			for _, p := range []string{v.FilePath, v.CoverPath} {
				if p == "" {
					continue
				}
				candidate := filepath.Join(abs, "media", filepath.FromSlash(p))
				rel, e := filepath.Rel(filepath.Join(abs, "media"), candidate)
				if e != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(p) {
					return fmt.Errorf("video %d has an unsafe media path", v.ID)
				}
				info, e := os.Stat(candidate)
				if e != nil || !info.Mode().IsRegular() {
					return fmt.Errorf("video %d media file missing: %s", v.ID, p)
				}
				if p == v.FilePath && info.Size() != v.Size {
					return fmt.Errorf("video %d media size mismatch", v.ID)
				}
			}
		}
		return nil
	}).Error
}

func copyLegacyRows[T any](source, target *gorm.DB) error {
	// These extension tables were introduced after the original release.
	var model T
	switch any(model).(type) {
	case FeedSnapshot, InteractionCommand:
		if !source.Migrator().HasTable(&model) {
			return nil
		}
	}
	var batch []T
	return source.FindInBatches(&batch, 200, func(_ *gorm.DB, _ int) error { return target.Create(&batch).Error }).Error
}

func transformRows[S any, D any](source, target *gorm.DB, convert func(S) D) error {
	var batch []S
	return source.FindInBatches(&batch, 200, func(_ *gorm.DB, _ int) error {
		out := make([]D, 0, len(batch))
		for _, v := range batch {
			out = append(out, convert(v))
		}
		if len(out) == 0 {
			return nil
		}
		return target.Create(&out).Error
	}).Error
}

func copyCoreData(s, t *gorm.DB) error {
	if err := transformRows(s, t, func(u User) account.UserModel {
		return account.UserModel{ID: int64(u.ID), Account: u.Username, Nickname: u.Username, Password: u.PasswordHash, Bio: u.Bio, AvatarURL: u.AvatarURL, Status: 1, Role: "user", CreatedAt: u.CreatedAt, UpdatedAt: u.CreatedAt}
	}); err != nil {
		return err
	}
	if err := transformRows(s, t, func(v Video) video.VideoModel {
		return video.VideoModel{ID: int64(v.ID), AuthorID: int64(v.UserID), Title: v.Title, Description: v.Description, MediaURL: "/media/" + v.FilePath, CoverURL: mediaURL(v.CoverPath), Status: 2, PublishedAt: &v.PublishedAt, CreatedAt: v.CreatedAt, UpdatedAt: v.CreatedAt, FilePath: v.FilePath, CoverPath: v.CoverPath, Size: v.Size}
	}); err != nil {
		return err
	}
	if err := transformRows(s, t, func(v Video) video.VideoStatModel {
		return video.VideoStatModel{VideoID: int64(v.ID), LikeCount: int(v.LikesCount), CommentCount: int(v.CommentsCount), CreatedAt: v.CreatedAt, UpdatedAt: v.CreatedAt}
	}); err != nil {
		return err
	}
	if err := transformRows(s, t, func(v Like) interaction.ActionModel {
		return interaction.ActionModel{ID: int64(v.ID), UserID: int64(v.UserID), VideoID: int64(v.VideoID), ActionType: "like", Status: 1, CreatedAt: v.CreatedAt, UpdatedAt: v.CreatedAt}
	}); err != nil {
		return err
	}
	if err := transformRows(s, t, func(v Comment) interaction.CommentModel {
		return interaction.CommentModel{ID: int64(v.ID), UserID: int64(v.UserID), VideoID: int64(v.VideoID), Content: v.Body, Status: 1, CreatedAt: v.CreatedAt, UpdatedAt: v.CreatedAt}
	}); err != nil {
		return err
	}
	if err := transformRows(s, t, func(v Follow) relation.FollowModel {
		return relation.FollowModel{ID: int64(v.ID), UserID: int64(v.FollowerID), TargetUserID: int64(v.FollowingID), Status: 1, CreatedAt: v.CreatedAt, UpdatedAt: v.CreatedAt}
	}); err != nil {
		return err
	}
	if err := transformRows(s, t, func(v Notification) message.MessageModel {
		return message.MessageModel{ID: int64(v.ID), UserID: int64(v.UserID), ActorID: int64(v.ActorID), VideoID: v.VideoID, Type: v.Kind, Title: v.Kind, Content: v.Body, IsRead: v.ReadAt != nil, ReadAt: v.ReadAt, CreatedAt: v.CreatedAt}
	}); err != nil {
		return err
	}
	if err := copyCoreUploads(s, t); err != nil {
		return err
	}
	for _, copy := range []func(*gorm.DB, *gorm.DB) error{copyLegacyRows[Invite], copyLegacyRows[Session], copyLegacyRows[VideoTag], copyLegacyRows[Message], copyLegacyRows[Outbox], copyLegacyRows[ProcessedEvent], copyLegacyRows[FeedSnapshot], copyLegacyRows[InteractionCommand]} {
		if err := copy(s, t); err != nil {
			return err
		}
	}
	return t.Exec(`INSERT INTO user_relation_stat (user_id,following_count,follower_count,created_at,updated_at) SELECT a.id,(SELECT COUNT(*) FROM user_follow f WHERE f.user_id=a.id AND f.status=1),(SELECT COUNT(*) FROM user_follow f WHERE f.target_user_id=a.id AND f.status=1),NOW(),NOW() FROM account a`).Error
}

func mediaURL(path string) string {
	if path == "" {
		return ""
	}
	return "/media/" + path
}

func finishCoreMigration(source, db *gorm.DB, counts map[string]int64) error {
	if err := installCoreViews(db); err != nil {
		return err
	}
	for table, want := range counts {
		var got int64
		if err := db.Table(table).Count(&got).Error; err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("verification failed for %s: got %d want %d", table, got, want)
		}
	}
	if err := verifyCoreContents(source, db); err != nil {
		return err
	}
	for _, query := range []string{
		`SELECT COUNT(*) FROM video v LEFT JOIN account a ON a.id=v.author_id WHERE a.id IS NULL`,
		`SELECT COUNT(*) FROM interaction_action x LEFT JOIN account a ON a.id=x.user_id LEFT JOIN video v ON v.id=x.video_id WHERE a.id IS NULL OR v.id IS NULL`,
		`SELECT COUNT(*) FROM interaction_comment x LEFT JOIN account a ON a.id=x.user_id LEFT JOIN video v ON v.id=x.video_id WHERE a.id IS NULL OR v.id IS NULL`,
		`SELECT COUNT(*) FROM video_stat s WHERE s.like_count<>(SELECT COUNT(*) FROM interaction_action a WHERE a.video_id=s.video_id AND a.action_type='like' AND a.status=1) OR s.comment_count<>(SELECT COUNT(*) FROM interaction_comment c WHERE c.video_id=s.video_id AND c.status=1)`,
	} {
		var n int64
		if err := db.Raw(query).Scan(&n).Error; err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("migration integrity check failed (%d rows)", n)
		}
	}
	service := embedding.New(embedrepo.New(db), nil)
	var rows []video.VideoModel
	if err := db.FindInBatches(&rows, 200, func(_ *gorm.DB, _ int) error {
		for _, v := range rows {
			if _, e := service.GenerateForPublishedVideo(context.Background(), &videoapp.PublishedEvent{VideoID: v.ID, Title: v.Title, Description: v.Description}); e != nil {
				return e
			}
		}
		return nil
	}).Error; err != nil {
		return err
	}
	if err := db.Model(&CoreMigration{}).Where("id = ?", coreBaseline).Update("state", "verified").Error; err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"state": "verified", "counts": counts, "baseline": coreBaseline, "sourceModified": false})
}

var coreVerifyColumns = map[string]string{
	"users":         "id,username,password_hash,bio,avatar_url,created_at",
	"videos":        "id,user_id,title,description,file_path,size,cover_path,likes_count,comments_count,published_at,created_at",
	"likes":         "id,user_id,video_id,created_at",
	"comments":      "id,user_id,video_id,body,created_at",
	"follows":       "id,follower_id,following_id,created_at",
	"notifications": "id,user_id,actor_id,video_id,kind,body,read_at,created_at",
	"messages":      "id,sender_id,recipient_id,body,created_at",
	"sessions":      "id,user_id,refresh_hash,expires_at,revoked_at,created_at",
	"invites":       "id,code_hash,used_by,expires_at,created_at",
	"uploads":       "id,user_id,file_md5,size,chunks,completed,published,stored_path,created_at",
	"video_tags":    "id,video_id,tag",
}

func verifyCoreContents(source, target *gorm.DB) error {
	for table, columns := range coreVerifyColumns {
		left, err := migrationDigest(source, table, columns, true)
		if err != nil {
			return err
		}
		right, err := migrationDigest(target, table, columns, false)
		if err != nil {
			return err
		}
		if left != right {
			return fmt.Errorf("content verification failed for %s", table)
		}
	}
	return nil
}

func migrationDigest(db *gorm.DB, table, columns string, source bool) (string, error) {
	// Names originate only in coreVerifyColumns, never from a request.
	rows, err := db.Raw("SELECT " + columns + " FROM " + table + " ORDER BY id").Rows()
	if err != nil {
		return "", err
	}
	defer rows.Close()
	h := sha256.New()
	n := len(strings.Split(columns, ","))
	for rows.Next() {
		values := make([]any, n)
		ptrs := make([]any, n)
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		for i, value := range values {
			if value == nil {
				io.WriteString(h, "N;")
				continue
			}
			io.WriteString(h, "V;")
			var text string
			switch v := value.(type) {
			case nil:
				text = "<NULL>"
			case []byte:
				text = string(v)
			case time.Time:
				text = v.UTC().Format(time.RFC3339Nano)
			default:
				text = fmt.Sprint(v)
			}
			if source && table == "uploads" && strings.Split(columns, ",")[i] == "stored_path" {
				text, err = migratedUploadPath(text)
				if err != nil {
					return "", err
				}
			}
			fmt.Fprintf(h, "%d:", len(text))
			io.WriteString(h, text)
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func migratedUploadPath(path string) (string, error) {
	if path == "" || path == "<NULL>" {
		return path, nil
	}
	source, err := filepath.Abs(env("SOURCE_RUNTIME_DATA_DIR", os.Getenv("SOURCE_DATA_DIR")))
	if err != nil {
		return "", err
	}
	target, err := filepath.Abs(env("TARGET_RUNTIME_DATA_DIR", os.Getenv("TARGET_DATA_DIR")))
	if err != nil {
		return "", err
	}
	original, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(source, original)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("upload stored_path lies outside SOURCE_RUNTIME_DATA_DIR")
	}
	return filepath.Join(target, rel), nil
}

func copyCoreUploads(source, target *gorm.DB) error {
	var rows []Upload
	return source.FindInBatches(&rows, 200, func(_ *gorm.DB, _ int) error {
		for i := range rows {
			path, err := migratedUploadPath(rows[i].StoredPath)
			if err != nil {
				return err
			}
			rows[i].StoredPath = path
		}
		return target.Create(&rows).Error
	}).Error
}
