package migration

import (
	infraaccount "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/account"
	infraembedding "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/embedding"
	infraexposure "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/exposure"
	infrafeed "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/feed"
	infrainteraction "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/interaction"
	inframessage "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/message"
	infraplayback "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/playback"
	infrarelation "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/relation"
	infravideo "github.com/ZoeySigel/owlet-video/backend/internal/infra/persistence/video"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
)

func AutoMigrate(db *gorm.DB) error {
	if err := autoMigrateModels(db); err != nil {
		return err
	}
	if err := infravideo.EnsureStats(db); err != nil {
		return err
	}
	return infrafeed.EnsureTimelineIndex(db)
}

func autoMigrateModels(db *gorm.DB) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		err = db.AutoMigrate(
			&infraaccount.UserModel{},
			&infraembedding.VideoEmbeddingModel{},
			&infravideo.VideoModel{},
			&infravideo.VideoStatModel{},
			&infrafeed.InboxModel{},
			&infraexposure.ViewEventModel{},
			&infraexposure.ExposureModel{},
			&infrainteraction.ActionModel{},
			&infrainteraction.CommentModel{},
			&inframessage.MessageModel{},
			&infraplayback.ConfigModel{},
			&infraplayback.QoSLogModel{},
			&infrarelation.FollowModel{},
			&infrarelation.RelationStatModel{},
		)
		if err == nil {
			return nil
		}
		if !isConcurrentMigrationError(err) {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 150 * time.Millisecond)
	}
	return err
}

func isConcurrentMigrationError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if !errors.As(err, &mysqlErr) {
		return false
	}
	switch mysqlErr.Number {
	case 1060, 1061:
		return true
	default:
		return false
	}
}
