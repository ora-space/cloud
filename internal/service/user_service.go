// Package service implements business logic.
package service

import (
	"context"
	"errors"

	"github.com/wanglongan587/cloud/internal/model"
	"github.com/wanglongan587/cloud/internal/repository"
)

// UserService defines interface for user business logic
type UserService interface {
	CreateUser(ctx context.Context, req *model.CreateUserRequest) (*model.User, error)
	GetUser(ctx context.Context, id uint) (*model.User, error)
	ListUsers(ctx context.Context, page, pageSize int) ([]*model.User, int64, error)
}

type userService struct {
	repo repository.UserRepository
}

// NewUserService creates a new UserService instance
func NewUserService(repo repository.UserRepository) UserService {
	return &userService{repo: repo}
}

func (s *userService) CreateUser(ctx context.Context, req *model.CreateUserRequest) (*model.User, error) {
	// Check if username already exists
	existing, _ := s.repo.GetByUsername(ctx, req.Username)
	if existing != nil {
		return nil, errors.New("username already exists")
	}

	user := &model.User{
		Username: req.Username,
		Nickname: req.Nickname,
		Email:    req.Email,
	}

	if err := s.repo.Create(ctx, user); err != nil {
		return nil, err
	}

	return user, nil
}

func (s *userService) GetUser(ctx context.Context, id uint) (*model.User, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *userService) ListUsers(ctx context.Context, page, pageSize int) ([]*model.User, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 10
	}
	offset := (page - 1) * pageSize

	return s.repo.List(ctx, offset, pageSize)
}
