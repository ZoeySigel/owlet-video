package main

import (
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const chunkSize int64 = 5 * 1024 * 1024
const maxVideoSize int64 = 200 * 1024 * 1024
const maxImageSize int64 = 10 * 1024 * 1024

func (a *App) uploadFor(c *gin.Context) (Upload, bool) {
	if _, err := uuid.Parse(c.Param("id")); err != nil {
		errorJSON(c, 400, "invalid_upload")
		return Upload{}, false
	}
	var u Upload
	if a.db.Where("id = ? AND user_id = ?", c.Param("id"), currentID(c)).First(&u).Error != nil {
		errorJSON(c, 404, "upload_not_found")
		return u, false
	}
	return u, true
}

func (a *App) initUpload(c *gin.Context) {
	var body struct {
		MD5    string `json:"md5"`
		Size   int64  `json:"size"`
		Chunks int    `json:"chunks"`
	}
	if c.ShouldBindJSON(&body) != nil || body.Size < 1 || body.Size > maxVideoSize || body.Chunks < 1 || body.Chunks != int((body.Size+chunkSize-1)/chunkSize) || len(body.MD5) != 32 {
		errorJSON(c, 400, "invalid_upload")
		return
	}
	if _, err := hex.DecodeString(body.MD5); err != nil {
		errorJSON(c, 400, "invalid_md5")
		return
	}
	var used int64
	a.db.Model(&Video{}).Select("COALESCE(SUM(size),0)").Scan(&used)
	if used+body.Size > a.cfg.MaxMediaBytes {
		errorJSON(c, 507, "media_capacity_reached")
		return
	}
	if free, err := freeBytes(a.cfg.DataDir); err == nil && free < uint64(8*1024*1024*1024+body.Size) {
		errorJSON(c, 507, "disk_space_low")
		return
	}
	u := Upload{ID: uuid.NewString(), UserID: currentID(c), FileMD5: strings.ToLower(body.MD5), Size: body.Size, Chunks: body.Chunks}
	if a.db.Create(&u).Error != nil {
		errorJSON(c, 500, "upload_init_failed")
		return
	}
	c.JSON(201, gin.H{"id": u.ID, "chunkSize": chunkSize})
}

func (a *App) uploadStatus(c *gin.Context) {
	u, ok := a.uploadFor(c)
	if !ok {
		return
	}
	indices := []int{}
	for i := 0; i < u.Chunks; i++ {
		if _, err := os.Stat(filepath.Join(a.cfg.DataDir, "tmp", u.ID, fmt.Sprintf("%03d", i))); err == nil {
			indices = append(indices, i)
		}
	}
	c.JSON(200, gin.H{"uploaded": indices, "completed": u.Completed, "published": u.Published})
}

func (a *App) uploadChunk(c *gin.Context) {
	u, ok := a.uploadFor(c)
	if !ok {
		return
	}
	if u.Completed {
		errorJSON(c, 409, "already_completed")
		return
	}
	i, err := strconv.Atoi(c.Param("index"))
	if err != nil || i < 0 || i >= u.Chunks {
		errorJSON(c, 400, "invalid_chunk")
		return
	}
	expected := chunkSize
	if i == u.Chunks-1 {
		expected = u.Size - int64(i)*chunkSize
	}
	if c.Request.ContentLength != expected {
		errorJSON(c, 400, "chunk_size_mismatch")
		return
	}
	dir := filepath.Join(a.cfg.DataDir, "tmp", u.ID)
	if os.MkdirAll(dir, 0700) != nil {
		errorJSON(c, 500, "storage_error")
		return
	}
	f, err := os.CreateTemp(dir, "incoming-")
	if err != nil {
		errorJSON(c, 500, "storage_error")
		return
	}
	defer os.Remove(f.Name())
	h := md5.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(c.Request.Body, expected+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil || n != expected || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), c.GetHeader("X-Chunk-MD5")) {
		errorJSON(c, 400, "chunk_checksum_mismatch")
		return
	}
	if os.Rename(f.Name(), filepath.Join(dir, fmt.Sprintf("%03d", i))) != nil {
		errorJSON(c, 500, "storage_error")
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func (a *App) completeUpload(c *gin.Context) {
	u, ok := a.uploadFor(c)
	if !ok {
		return
	}
	if u.Completed {
		c.JSON(200, gin.H{"id": u.ID})
		return
	}
	dir := filepath.Join(a.cfg.DataDir, "tmp", u.ID)
	f, err := os.CreateTemp(filepath.Join(a.cfg.DataDir, "tmp"), "assembled-")
	if err != nil {
		errorJSON(c, 500, "storage_error")
		return
	}
	defer os.Remove(f.Name())
	h := md5.New()
	var total int64
	for i := 0; i < u.Chunks; i++ {
		part, err := os.Open(filepath.Join(dir, fmt.Sprintf("%03d", i)))
		if err != nil {
			f.Close()
			errorJSON(c, 409, "missing_chunk")
			return
		}
		n, e := io.Copy(io.MultiWriter(f, h), part)
		part.Close()
		if e != nil {
			f.Close()
			errorJSON(c, 500, "storage_error")
			return
		}
		total += n
	}
	if total != u.Size || !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), u.FileMD5) {
		f.Close()
		errorJSON(c, 400, "file_checksum_mismatch")
		return
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		errorJSON(c, 500, "storage_error")
		return
	}
	header := make([]byte, 12)
	_, err = io.ReadFull(f, header)
	if err != nil || string(header[4:8]) != "ftyp" {
		f.Close()
		errorJSON(c, 400, "mp4_required")
		return
	}
	if f.Close() != nil {
		errorJSON(c, 500, "storage_error")
		return
	}
	path := filepath.Join(a.cfg.DataDir, "tmp", u.ID+".mp4")
	if os.Rename(f.Name(), path) != nil {
		errorJSON(c, 500, "storage_error")
		return
	}
	if a.db.Model(&u).Updates(map[string]any{"completed": true, "stored_path": path}).Error != nil {
		errorJSON(c, 500, "upload_complete_failed")
		return
	}
	_ = os.RemoveAll(dir)
	c.JSON(200, gin.H{"id": u.ID})
}

func (a *App) saveImage(c *gin.Context, folder string) (string, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxImageSize+1024)
	file, _, err := c.Request.FormFile("file")
	if err != nil {
		return "", err
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, maxImageSize+1))
	if err != nil || int64(len(b)) > maxImageSize {
		return "", errors.New("image too large")
	}
	mediaType := http.DetectContentType(b)
	ext := ""
	switch mediaType {
	case "image/jpeg":
		ext = ".jpg"
	case "image/png":
		ext = ".png"
	case "image/webp":
		ext = ".webp"
	default:
		return "", errors.New("unsupported image")
	}
	dir := filepath.Join(a.cfg.DataDir, folder)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return "", err
	}
	name := uuid.NewString() + ext
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0640); err != nil {
		return "", err
	}
	return filepath.ToSlash(filepath.Join(folder, name)), nil
}

func (a *App) uploadAvatar(c *gin.Context) {
	path, err := a.saveImage(c, "media/avatars")
	if err != nil {
		errorJSON(c, 400, "invalid_image")
		return
	}
	url := "/" + strings.TrimPrefix(path, "media/")
	if err := a.db.Model(&User{}).Where("id = ?", currentID(c)).Update("avatar_url", "/media/"+strings.TrimPrefix(path, "media/")).Error; err != nil {
		errorJSON(c, 500, "update_failed")
		return
	}
	c.JSON(200, gin.H{"url": "/media" + url})
}

func (a *App) uploadCover(c *gin.Context) {
	path, err := a.saveImage(c, filepath.Join("tmp", "covers", strconv.FormatUint(uint64(currentID(c)), 10)))
	if err != nil {
		errorJSON(c, 400, "invalid_image")
		return
	}
	c.JSON(201, gin.H{"coverId": filepath.Base(path)})
}

var tagPattern = regexp.MustCompile(`#[\p{L}\p{N}_]{1,60}`)

func (a *App) publishVideo(c *gin.Context) {
	var body struct{ UploadID, Title, Description, CoverID string }
	if c.ShouldBindJSON(&body) != nil || len(strings.TrimSpace(body.Title)) < 1 || len(body.Title) > 160 || len(body.Description) > 2000 {
		errorJSON(c, 400, "invalid_video")
		return
	}
	if _, err := uuid.Parse(body.UploadID); err != nil {
		errorJSON(c, 400, "invalid_upload")
		return
	}
	var u Upload
	if a.db.Where("id = ? AND user_id = ? AND completed = ? AND published = ?", body.UploadID, currentID(c), true, false).First(&u).Error != nil {
		errorJSON(c, 404, "upload_not_found")
		return
	}
	if _, err := os.Stat(u.StoredPath); err != nil {
		errorJSON(c, 409, "upload_missing")
		return
	}
	coverPath := ""
	if body.CoverID != "" {
		if strings.ContainsAny(body.CoverID, "/\\") || len(body.CoverID) > 60 {
			errorJSON(c, 400, "invalid_cover")
			return
		}
		candidate := filepath.Join(a.cfg.DataDir, "tmp", "covers", strconv.FormatUint(uint64(currentID(c)), 10), body.CoverID)
		if _, err := os.Stat(candidate); err != nil {
			errorJSON(c, 400, "cover_not_found")
			return
		}
		coverPath = filepath.ToSlash(filepath.Join("covers", body.CoverID))
	}
	name := uuid.NewString() + ".mp4"
	rel := filepath.ToSlash(filepath.Join("videos", name))
	dest := filepath.Join(a.cfg.DataDir, "media", filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dest), 0750); err != nil {
		errorJSON(c, 500, "storage_error")
		return
	}
	if err := os.Rename(u.StoredPath, dest); err != nil {
		errorJSON(c, 500, "storage_error")
		return
	}
	if err := os.Chmod(dest, 0640); err != nil {
		_ = os.Rename(dest, u.StoredPath)
		errorJSON(c, 500, "storage_error")
		return
	}
	if coverPath != "" {
		src := filepath.Join(a.cfg.DataDir, "tmp", "covers", strconv.FormatUint(uint64(currentID(c)), 10), body.CoverID)
		_ = os.MkdirAll(filepath.Join(a.cfg.DataDir, "media", "covers"), 0750)
		if err := os.Rename(src, filepath.Join(a.cfg.DataDir, "media", filepath.FromSlash(coverPath))); err != nil {
			_ = os.Rename(dest, u.StoredPath)
			errorJSON(c, 500, "storage_error")
			return
		}
	}
	v := Video{UserID: currentID(c), Title: strings.TrimSpace(body.Title), Description: body.Description, FilePath: rel, CoverPath: coverPath, Size: u.Size, PublishedAt: time.Now()}
	err := a.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&v).Error; err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, raw := range tagPattern.FindAllString(body.Description, -1) {
			tag := strings.ToLower(strings.TrimPrefix(raw, "#"))
			if !seen[tag] {
				seen[tag] = true
				if err := tx.Create(&VideoTag{VideoID: v.ID, Tag: tag}).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Model(&Upload{}).Where("id = ? AND published = ?", u.ID, false).Update("published", true).Error; err != nil {
			return err
		}
		return enqueue(tx, "video.published", map[string]any{"videoId": v.ID, "userId": v.UserID})
	})
	if err != nil {
		_ = os.Rename(dest, u.StoredPath)
		errorJSON(c, 500, "publish_failed")
		return
	}
	a.hydrateVideo(&v)
	c.JSON(201, v)
}

func (a *App) hydrateVideo(v *Video) {
	v.PlayURL = "/media/" + v.FilePath
	if v.CoverPath != "" {
		v.CoverURL = "/media/" + v.CoverPath
	}
}
func (a *App) videoDetail(c *gin.Context) {
	id, ok := paramID(c, "id")
	if !ok {
		return
	}
	result, err := a.detail(c.Request.Context(), id)
	if err != nil {
		c.Header("Retry-After", "1")
		errorJSON(c, 503, "video_temporarily_unavailable")
		return
	}
	if result.value.Missing {
		errorJSON(c, 404, "video_not_found")
		return
	}
	if result.stale {
		c.Header("X-Cache", "stale")
	} else {
		c.Header("X-Cache", "fresh")
	}
	c.JSON(200, result.value.Video)
}

func (a *App) userVideos(c *gin.Context) {
	var videos []Video
	q := a.db.Preload("User").Where("user_id = ?", c.Param("id")).Order("id DESC").Limit(30)
	if cursor := c.Query("cursor"); cursor != "" {
		q = q.Where("id < ?", cursor)
	}
	if q.Find(&videos).Error != nil {
		errorJSON(c, 500, "query_failed")
		return
	}
	for i := range videos {
		a.hydrateVideo(&videos[i])
	}
	c.JSON(200, videos)
}

func (a *App) cleanupUploads() error {
	var stale []Upload
	if err := a.db.Where("created_at < ? AND published = ?", time.Now().Add(-48*time.Hour), false).Find(&stale).Error; err != nil {
		return err
	}
	for _, u := range stale {
		_ = os.RemoveAll(filepath.Join(a.cfg.DataDir, "tmp", u.ID))
		if u.StoredPath != "" {
			_ = os.Remove(u.StoredPath)
		}
		if err := a.db.Delete(&u).Error; err != nil {
			return err
		}
	}
	return nil
}
