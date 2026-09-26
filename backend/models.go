package main

import "time"

type User struct {
	ID           uint      `gorm:"primaryKey" json:"id"`
	Username     string    `gorm:"size:40;uniqueIndex;not null" json:"username"`
	PasswordHash string    `gorm:"size:255;not null" json:"-"`
	Bio          string    `gorm:"size:500" json:"bio"`
	AvatarURL    string    `gorm:"size:300" json:"avatarUrl"`
	CreatedAt    time.Time `json:"createdAt"`
}
type Invite struct {
	ID        uint   `gorm:"primaryKey"`
	CodeHash  string `gorm:"size:64;uniqueIndex;not null"`
	UsedBy    *uint
	ExpiresAt time.Time
	CreatedAt time.Time
}
type Session struct {
	ID          string    `gorm:"primaryKey;size:36"`
	UserID      uint      `gorm:"index;not null"`
	RefreshHash string    `gorm:"size:64;not null"`
	ExpiresAt   time.Time `gorm:"index"`
	RevokedAt   *time.Time
	CreatedAt   time.Time
}
type Video struct {
	ID            uint      `gorm:"primaryKey" json:"id"`
	UserID        uint      `gorm:"index:idx_video_user_published;not null" json:"userId"`
	User          User      `gorm:"foreignKey:UserID" json:"author"`
	Title         string    `gorm:"size:160;not null" json:"title"`
	Description   string    `gorm:"size:2000" json:"description"`
	FilePath      string    `gorm:"size:300;not null" json:"-"`
	Size          int64     `gorm:"not null" json:"size"`
	CoverPath     string    `gorm:"size:300" json:"-"`
	PlayURL       string    `gorm:"-" json:"playUrl"`
	CoverURL      string    `gorm:"-" json:"coverUrl"`
	LikesCount    int64     `gorm:"not null;default:0;index:idx_video_likes" json:"likesCount"`
	CommentsCount int64     `gorm:"not null;default:0" json:"commentsCount"`
	Popularity    float64   `gorm:"not null;default:0;index:idx_video_popularity" json:"popularity"`
	PublishedAt   time.Time `gorm:"index:idx_video_user_published;index:idx_video_published" json:"publishedAt"`
	CreatedAt     time.Time `json:"-"`
}
type Upload struct {
	ID         string `gorm:"primaryKey;size:36"`
	UserID     uint   `gorm:"index;not null"`
	FileMD5    string `gorm:"size:32;not null"`
	Size       int64  `gorm:"not null"`
	Chunks     int    `gorm:"not null"`
	Completed  bool   `gorm:"not null;default:false"`
	Published  bool   `gorm:"not null;default:false"`
	StoredPath string `gorm:"size:300"`
	CreatedAt  time.Time
}
type Like struct {
	ID        uint      `gorm:"primaryKey"`
	UserID    uint      `gorm:"uniqueIndex:idx_like_user_video;not null"`
	VideoID   uint      `gorm:"uniqueIndex:idx_like_user_video;index;index:idx_like_created_video,priority:2;not null"`
	CreatedAt time.Time `gorm:"index:idx_like_created_video,priority:1"`
}
type Comment struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"index;not null" json:"userId"`
	User      User      `gorm:"foreignKey:UserID" json:"author"`
	VideoID   uint      `gorm:"index:idx_comment_video_created;not null" json:"videoId"`
	Body      string    `gorm:"size:1000;not null" json:"body"`
	CreatedAt time.Time `gorm:"index:idx_comment_video_created;index:idx_comment_created" json:"createdAt"`
}
type Follow struct {
	ID          uint `gorm:"primaryKey"`
	FollowerID  uint `gorm:"uniqueIndex:idx_follow_pair;index;not null"`
	FollowingID uint `gorm:"uniqueIndex:idx_follow_pair;index;not null"`
	CreatedAt   time.Time
}
type VideoTag struct {
	ID      uint   `gorm:"primaryKey"`
	VideoID uint   `gorm:"uniqueIndex:idx_video_tag;not null"`
	Tag     string `gorm:"size:60;uniqueIndex:idx_video_tag;index;not null"`
}
type Message struct {
	ID          uint      `gorm:"primaryKey" json:"id"`
	SenderID    uint      `gorm:"index;not null" json:"senderId"`
	RecipientID uint      `gorm:"index;not null" json:"recipientId"`
	Body        string    `gorm:"size:2000;not null" json:"body"`
	CreatedAt   time.Time `gorm:"index" json:"createdAt"`
}
type Notification struct {
	ID        uint       `gorm:"primaryKey" json:"id"`
	UserID    uint       `gorm:"index:idx_notice_user_created;not null" json:"userId"`
	ActorID   uint       `json:"actorId"`
	VideoID   *uint      `json:"videoId"`
	Kind      string     `gorm:"size:30;not null" json:"kind"`
	Body      string     `gorm:"size:300" json:"body"`
	ReadAt    *time.Time `json:"readAt"`
	CreatedAt time.Time  `gorm:"index:idx_notice_user_created" json:"createdAt"`
}
type Outbox struct {
	ID          string     `gorm:"primaryKey;size:36"`
	Kind        string     `gorm:"size:50;not null"`
	Payload     []byte     `gorm:"type:blob;not null"`
	PublishedAt *time.Time `gorm:"index;index:idx_outbox_pending_created,priority:1"`
	CreatedAt   time.Time  `gorm:"index:idx_outbox_pending_created,priority:2"`
}
type ProcessedEvent struct {
	ID        string `gorm:"primaryKey;size:36"`
	CreatedAt time.Time
}

// Ranking snapshots keep pagination stable even when live counters change.
// MySQL is authoritative so Redis loss does not invalidate an issued cursor.
type FeedSnapshot struct {
	ID        string `gorm:"primaryKey;size:36"`
	Sort      string `gorm:"size:16;not null"`
	Entries   []byte `gorm:"type:mediumblob;not null"`
	CreatedAt time.Time
	ExpiresAt time.Time `gorm:"index;not null"`
}
