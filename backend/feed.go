package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (a *App) optionalUser(c *gin.Context) uint {
	raw,err:=c.Cookie("owlet_access");if err!=nil{return 0}
	claims,err:=a.parseToken(raw,"access");if err!=nil{return 0}
	id,err:=strconv.ParseUint(claims.Subject,10,64);if err!=nil{return 0}
	var n int64;a.db.Model(&Session{}).Where("id = ? AND user_id = ? AND revoked_at IS NULL AND expires_at > ?",claims.SessionID,id,time.Now()).Count(&n)
	if n==0{return 0};return uint(id)
}

func decodeCursor(raw string) (float64,uint,error) {
	if raw==""{return 0,0,nil}
	b,err:=base64.RawURLEncoding.DecodeString(raw);if err!=nil{return 0,0,err}
	parts:=strings.Split(string(b),":");if len(parts)!=2{return 0,0,fmt.Errorf("invalid cursor")}
	score,err:=strconv.ParseFloat(parts[0],64);if err!=nil{return 0,0,err}
	id,err:=strconv.ParseUint(parts[1],10,64);return score,uint(id),err
}
func makeCursor(score float64,id uint)string{return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%g:%d",score,id)))}

func (a *App) feed(c *gin.Context) {
	sort:=c.DefaultQuery("sort","latest")
	if sort!="latest"&&sort!="hot"&&sort!="likes"&&sort!="following"{errorJSON(c,400,"invalid_sort");return}
	score,id,err:=decodeCursor(c.Query("cursor"));if err!=nil{errorJSON(c,400,"invalid_cursor");return}
	q:=a.db.Preload("User").Model(&Video{})
	switch sort {
	case "latest":
		if id>0{q=q.Where("id < ?",id)}
		q=q.Order("id DESC")
	case "following":
		uid:=a.optionalUser(c);if uid==0{errorJSON(c,401,"login_required");return}
		q=q.Where("user_id IN (?)",a.db.Model(&Follow{}).Select("following_id").Where("follower_id = ?",uid))
		if id>0{q=q.Where("id < ?",id)}
		q=q.Order("id DESC")
	case "hot":
		if id>0{q=q.Where("popularity < ? OR (popularity = ? AND id < ?)",score,score,id)}
		q=q.Order("popularity DESC, id DESC")
	case "likes":
		if id>0{q=q.Where("likes_count < ? OR (likes_count = ? AND id < ?)",int64(score),int64(score),id)}
		q=q.Order("likes_count DESC, id DESC")
	}
	var videos []Video
	if q.Limit(21).Find(&videos).Error!=nil{errorJSON(c,500,"feed_failed");return}
	next:="";if len(videos)>20{videos=videos[:20];last:=videos[len(videos)-1];rank:=float64(0);if sort=="hot"{rank=last.Popularity};if sort=="likes"{rank=float64(last.LikesCount)};next=makeCursor(rank,last.ID)}
	for i:=range videos{a.hydrateVideo(&videos[i])}
	c.JSON(200,gin.H{"items":videos,"nextCursor":next})
}

func (a *App) tagFeed(c *gin.Context) {
	tag:=strings.ToLower(c.Param("tag"));if len(tag)<1||len(tag)>60{errorJSON(c,400,"invalid_tag");return}
	var videos []Video
	q:=a.db.Preload("User").Where("id IN (?)",a.db.Model(&VideoTag{}).Select("video_id").Where("tag = ?",tag)).Order("id DESC").Limit(20)
	if cursor:=c.Query("cursor");cursor!=""{q=q.Where("id < ?",cursor)}
	if q.Find(&videos).Error!=nil{errorJSON(c,500,"feed_failed");return}
	for i:=range videos{a.hydrateVideo(&videos[i])};c.JSON(200,videos)
}

func (a *App) cachedVideo(id string) (Video,bool) {
	var v Video
	b,err:=a.redis.Get(context.Background(),"video:"+id).Bytes();if err!=nil||json.Unmarshal(b,&v)!=nil{return v,false};return v,true
}

func (a *App) cacheVideo(v Video) {
	b,err:=json.Marshal(v);if err==nil{_ = a.redis.Set(context.Background(),"video:"+strconv.FormatUint(uint64(v.ID),10),b,5*time.Minute).Err()}
}

func (a *App) invalidateVideo(id uint) {
	_ = a.redis.Del(context.Background(),"video:"+strconv.FormatUint(uint64(id),10)).Err()
}
