package main

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func paramID(c *gin.Context, key string) (uint,bool) {
	id,err:=strconv.ParseUint(c.Param(key),10,64)
	if err!=nil||id==0{errorJSON(c,400,"invalid_id");return 0,false}
	return uint(id),true
}

func (a *App) likeVideo(c *gin.Context) {
	id,ok:=paramID(c,"id");if !ok{return};uid:=currentID(c)
	err:=a.db.Transaction(func(tx *gorm.DB) error {
		var v Video;if err:=tx.First(&v,id).Error;err!=nil{return err}
		var exists int64;tx.Model(&Like{}).Where("user_id = ? AND video_id = ?",uid,id).Count(&exists);if exists>0{return nil}
		if err:=tx.Create(&Like{UserID:uid,VideoID:id}).Error;err!=nil{return err}
		if err:=tx.Model(&Video{}).Where("id = ?",id).Updates(map[string]any{"likes_count":gorm.Expr("likes_count + 1"),"popularity":gorm.Expr("popularity + 3")}).Error;err!=nil{return err}
		return enqueue(tx,"video.liked",map[string]any{"videoId":id,"userId":uid,"ownerId":v.UserID})
	})
	if err!=nil{errorJSON(c,500,"like_failed");return};a.invalidateVideo(id);c.JSON(200,gin.H{"liked":true})
}

func (a *App) unlikeVideo(c *gin.Context) {
	id,ok:=paramID(c,"id");if !ok{return};uid:=currentID(c)
	err:=a.db.Transaction(func(tx *gorm.DB) error {
		result:=tx.Where("user_id = ? AND video_id = ?",uid,id).Delete(&Like{});if result.Error!=nil{return result.Error};if result.RowsAffected==0{return nil}
		if err:=tx.Model(&Video{}).Where("id = ?",id).Updates(map[string]any{"likes_count":gorm.Expr("GREATEST(likes_count - 1, 0)"),"popularity":gorm.Expr("GREATEST(popularity - 3, 0)")}).Error;err!=nil{return err}
		return enqueue(tx,"video.unliked",map[string]any{"videoId":id})
	})
	if err!=nil{errorJSON(c,500,"unlike_failed");return};a.invalidateVideo(id);c.JSON(200,gin.H{"liked":false})
}

func (a *App) isLiked(c *gin.Context) {
	id,ok:=paramID(c,"id");if !ok{return}
	var n int64;a.db.Model(&Like{}).Where("user_id = ? AND video_id = ?",currentID(c),id).Count(&n)
	c.JSON(200,gin.H{"liked":n>0})
}

func (a *App) myLikes(c *gin.Context) {
	var videos []Video
	q:=a.db.Preload("User").Where("id IN (?)",a.db.Model(&Like{}).Select("video_id").Where("user_id = ?",currentID(c))).Order("id DESC").Limit(30)
	if q.Find(&videos).Error!=nil{errorJSON(c,500,"query_failed");return}
	for i:=range videos{a.hydrateVideo(&videos[i])};c.JSON(200,videos)
}

func (a *App) comments(c *gin.Context) {
	id,ok:=paramID(c,"id");if !ok{return}
	var list []Comment;q:=a.db.Preload("User").Where("video_id = ?",id).Order("id ASC").Limit(50)
	if cursor:=c.Query("cursor");cursor!=""{q=q.Where("id > ?",cursor)}
	if q.Find(&list).Error!=nil{errorJSON(c,500,"query_failed");return};c.JSON(200,list)
}

func (a *App) createComment(c *gin.Context) {
	id,ok:=paramID(c,"id");if !ok{return}
	var body struct{ Body string `json:"body"` };if c.ShouldBindJSON(&body)!=nil{errorJSON(c,400,"invalid_comment");return}
	body.Body=strings.TrimSpace(body.Body);if len(body.Body)<1||len(body.Body)>1000{errorJSON(c,400,"invalid_comment");return}
	comment:=Comment{UserID:currentID(c),VideoID:id,Body:body.Body}
	err:=a.db.Transaction(func(tx *gorm.DB) error {
		var v Video;if err:=tx.First(&v,id).Error;err!=nil{return err}
		if err:=tx.Create(&comment).Error;err!=nil{return err}
		if err:=tx.Model(&Video{}).Where("id = ?",id).Updates(map[string]any{"comments_count":gorm.Expr("comments_count + 1"),"popularity":gorm.Expr("popularity + 5")}).Error;err!=nil{return err}
		return enqueue(tx,"video.commented",map[string]any{"videoId":id,"userId":currentID(c),"ownerId":v.UserID,"body":body.Body})
	})
	if err!=nil{errorJSON(c,500,"comment_failed");return};a.invalidateVideo(id)
	a.db.Preload("User").First(&comment,comment.ID);c.JSON(201,comment)
}

func (a *App) deleteComment(c *gin.Context) {
	id,ok:=paramID(c,"id");if !ok{return}
	var videoID uint
	err:=a.db.Transaction(func(tx *gorm.DB) error {
		var comment Comment
		if err:=tx.Where("id = ? AND user_id = ?",id,currentID(c)).First(&comment).Error;err!=nil{return err}
		videoID=comment.VideoID
		if err:=tx.Delete(&comment).Error;err!=nil{return err}
		if err:=tx.Model(&Video{}).Where("id = ?",videoID).Updates(map[string]any{"comments_count":gorm.Expr("GREATEST(comments_count - 1, 0)"),"popularity":gorm.Expr("GREATEST(popularity - 5, 0)")}).Error;err!=nil{return err}
		return enqueue(tx,"video.comment_deleted",map[string]any{"videoId":videoID})
	})
	if err!=nil{errorJSON(c,404,"comment_not_found");return};a.invalidateVideo(videoID);c.Status(http.StatusNoContent)
}

func (a *App) follow(c *gin.Context) {
	id,ok:=paramID(c,"id");if !ok{return};uid:=currentID(c);if id==uid{errorJSON(c,400,"cannot_follow_self");return}
	err:=a.db.Transaction(func(tx *gorm.DB) error {
		var user User;if err:=tx.First(&user,id).Error;err!=nil{return err}
		var n int64;tx.Model(&Follow{}).Where("follower_id = ? AND following_id = ?",uid,id).Count(&n);if n>0{return nil}
		if err:=tx.Create(&Follow{FollowerID:uid,FollowingID:id}).Error;err!=nil{return err}
		return enqueue(tx,"user.followed",map[string]any{"userId":uid,"ownerId":id})
	})
	if err!=nil{errorJSON(c,404,"user_not_found");return};c.JSON(200,gin.H{"following":true})
}

func (a *App) unfollow(c *gin.Context) {
	id,ok:=paramID(c,"id");if !ok{return}
	if a.db.Where("follower_id = ? AND following_id = ?",currentID(c),id).Delete(&Follow{}).Error!=nil{errorJSON(c,500,"unfollow_failed");return}
	c.JSON(200,gin.H{"following":false})
}

func (a *App) relationList(c *gin.Context, followers bool) {
	id,ok:=paramID(c,"id");if !ok{return}
	var ids []uint;q:=a.db.Model(&Follow{}).Limit(100)
	if followers{q=q.Select("follower_id").Where("following_id = ?",id)}else{q=q.Select("following_id").Where("follower_id = ?",id)}
	if q.Scan(&ids).Error!=nil{errorJSON(c,500,"query_failed");return}
	list:=[]User{};if len(ids)>0{a.db.Where("id IN ?",ids).Find(&list)};c.JSON(200,list)
}
func (a *App) followers(c *gin.Context){a.relationList(c,true)}
func (a *App) following(c *gin.Context){a.relationList(c,false)}
