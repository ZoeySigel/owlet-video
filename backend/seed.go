package main

import (
	cryptorand "crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

//go:embed testdata/seed/*.mp4
var seedClips embed.FS

var seedClipNames = []string{
	"clip-0.mp4", "clip-60.mp4", "clip-120.mp4",
	"clip-180.mp4", "clip-240.mp4", "clip-300.mp4",
}

var seedTitles = []string{
	"霓虹色彩练习", "像素画面循环", "两秒调色板", "跳动的几何图案",
	"复古屏幕测试", "彩色图形实验", "短视频播放测试", "不同色相的瞬间",
	"抽象画面记录", "色块运动实验", "测试素材一角", "小小视觉练习",
}

var seedTags = []string{"日常", "城市", "色彩", "光影", "随拍", "测试"}
var seedComments = []string{"这个画面真不错！", "喜欢这个配色。", "拍得很有意思。", "收藏了，下次再看。"}

func seedRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := cryptorand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func copySeedClip(dst, name string) (int64, error) {
	src, err := seedClips.Open("testdata/seed/" + name)
	if err != nil {
		return 0, err
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0640)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(out, src)
	closeErr := out.Close()
	if copyErr != nil {
		return 0, copyErr
	}
	return n, closeErr
}

func (a *App) seedTestData(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	usersCount := fs.Int("users", 6, "number of test accounts (2-30)")
	videosCount := fs.Int("videos", 24, "number of test videos (1-200)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *usersCount < 2 || *usersCount > 30 || *videosCount < 1 || *videosCount > 200 {
		return errors.New("usage: seed [-users 2..30] [-videos 1..200]")
	}
	dir := filepath.Join(a.cfg.DataDir, "media", "videos")
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	type account struct {
		username, password string
		id                 uint
	}
	accounts := make([]account, *usersCount)
	for i := range accounts {
		suffix, err := seedRandomHex(4)
		if err != nil {
			return err
		}
		password, err := seedRandomHex(12)
		if err != nil {
			return err
		}
		accounts[i] = account{username: "demo_" + suffix, password: password}
	}

	// Files are written before the transaction and removed if any database step fails.
	files := make([]string, 0, *videosCount)
	committed := false
	defer func() {
		if !committed {
			for _, path := range files {
				_ = os.Remove(path)
			}
		}
	}()
	videos := make([]Video, *videosCount)
	for i := range videos {
		name, err := seedRandomHex(16)
		if err != nil {
			return err
		}
		rel := filepath.ToSlash(filepath.Join("videos", "seed-"+name+".mp4"))
		path := filepath.Join(a.cfg.DataDir, "media", filepath.FromSlash(rel))
		size, err := copySeedClip(path, seedClipNames[i%len(seedClipNames)])
		if err != nil {
			_ = os.Remove(path)
			return err
		}
		files = append(files, path)
		tag := seedTags[i%len(seedTags)]
		videos[i] = Video{
			Title:       fmt.Sprintf("%s · %02d", seedTitles[i%len(seedTitles)], i+1),
			Description: fmt.Sprintf("本地演示视频 #%s #测试", tag),
			FilePath:    rel, Size: size,
			PublishedAt: time.Now().Add(-time.Duration(*videosCount-i) * 3 * time.Hour),
		}
	}

	err := a.db.Transaction(func(tx *gorm.DB) error {
		for i := range accounts {
			hash, err := bcrypt.GenerateFromPassword([]byte(accounts[i].password), bcrypt.DefaultCost)
			if err != nil {
				return err
			}
			u := User{Username: accounts[i].username, PasswordHash: string(hash), Bio: "本地测试账号 · 用于体验 Owlet Video"}
			if err := tx.Create(&u).Error; err != nil {
				return err
			}
			accounts[i].id = u.ID
		}
		for i := range videos {
			v := &videos[i]
			v.UserID = accounts[i%len(accounts)].id
			if err := tx.Create(v).Error; err != nil {
				return err
			}
			seen := map[string]bool{}
			for _, raw := range tagPattern.FindAllString(v.Description, -1) {
				tag := strings.ToLower(strings.TrimPrefix(raw, "#"))
				if seen[tag] {
					continue
				}
				seen[tag] = true
				if err := tx.Create(&VideoTag{VideoID: v.ID, Tag: tag}).Error; err != nil {
					return err
				}
			}
			for j := range accounts {
				if accounts[j].id == v.UserID || rand.IntN(3) == 0 {
					continue
				}
				if err := tx.Create(&Like{UserID: accounts[j].id, VideoID: v.ID}).Error; err != nil {
					return err
				}
				v.LikesCount++
			}
			commentCount := rand.IntN(3)
			for j := 0; j < commentCount; j++ {
				actor := accounts[(i+j+1)%len(accounts)]
				body := seedComments[rand.IntN(len(seedComments))]
				if err := tx.Create(&Comment{UserID: actor.id, VideoID: v.ID, Body: body}).Error; err != nil {
					return err
				}
				v.CommentsCount++
			}
			v.Popularity = float64(v.LikesCount*3 + v.CommentsCount*5)
			if err := tx.Model(v).Updates(map[string]any{"likes_count": v.LikesCount, "comments_count": v.CommentsCount, "popularity": v.Popularity}).Error; err != nil {
				return err
			}
		}
		for i := range accounts {
			for offset := 1; offset <= 2 && offset < len(accounts); offset++ {
				if err := tx.Create(&Follow{FollowerID: accounts[i].id, FollowingID: accounts[(i+offset)%len(accounts)].id}).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	committed = true
	fmt.Printf("Created %d test accounts and %d playable videos.\n", len(accounts), len(videos))
	fmt.Println("Login credentials (save these now; passwords are not stored in plaintext):")
	for _, account := range accounts {
		fmt.Printf("  %s  %s\n", account.username, account.password)
	}
	return nil
}
