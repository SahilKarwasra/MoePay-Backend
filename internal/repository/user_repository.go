package repository

import (
	"context"

	"github.com/sahilkarwasra/moepay/internal/models"
)

type UserRepository interface {
	CreateUser(ctx context.Context, user *models.User) error
	GetUserByEmail(ctx context.Context, email string) (*models.User, error)
	GetUserByID(ctx context.Context, userID string) (*models.User, error)
	GetUserByPhoneNumber(ctx context.Context, phoneNumber string) (*models.User, error)
	GetUserByWalletID(ctx context.Context, walletID string) (*models.User, error)
	UpdateUser(ctx context.Context, user *models.User) error
	DeleteUser(ctx context.Context, userID string) error
	EnsureIndexes(ctx context.Context) error
}
