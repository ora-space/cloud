package model

import (
	"time"

	"gorm.io/gorm"
)

// User represents user entity in the database
type User struct {
	ID        uint           `gorm:"0" json:"id"`
	Username  string         `gorm:"size:64;not null;uniqueIndex" json:"username"`
	Nickname  string         `gorm:"size:64" json:"nickname"`
	Email     string         `gorm:"size:128;uniqueIndex" json:"email"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// TableName sets table name for User
func (User) TableName() string {
	return "users"
}

// CreateUserRequest defines request payload for creating user
type CreateUserRequest struct {
	Username string `json:"username" binding:"required,min=3,max=32"`
	Nickname string `json:"nickname" binding:"max=32"`
	Email    string `json:"email" binding:"required,email"`
}
