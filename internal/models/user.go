package models

import (
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

type User struct {
	ID          primitive.ObjectID `bson:"_id,omitempty" json:"id"`
	PhoneNumber string             `bson:"phone_number" json:"phone_number"`
	CreatedAt   time.Time          `bson:"created_at" json:"created_at"`
	UpdatedAt   time.Time          `bson:"updated_at" json:"updated_at"`
	UserName    string             `bson:"user_name" json:"user_name"`
	WalletId    string             `bson:"wallet_id" json:"wallet_id"`
	KycStatus   string             `bson:"kyc_status" json:"kyc_status"`
}

type UpdateUserRequest struct {
	UserName string `json:"user_name" binding:"required"`
	WalletId string `json:"wallet_id" binding:"required"`
}

type PublicUser struct {
	ID          string    `json:"id" bson:"id"`
	UserName    string    `json:"user_name" bson:"user_name"`
	PhoneNumber string    `json:"phone_number" bson:"phone_number"`
	WalletId    string    `json:"wallet_id" bson:"wallet_id"`
	KycStatus   string    `json:"kyc_status" bson:"kyc_status"`
	CreatedAt   time.Time `json:"created_at" bson:"created_at"`
	UpdatedAt   time.Time `json:"updated_at" bson:"updated_at"`
}

func (u *User) ToPublic() PublicUser {
	return PublicUser{
		ID:          u.ID.Hex(),
		UserName:    u.UserName,
		PhoneNumber: u.PhoneNumber,
		WalletId:    u.WalletId,
		KycStatus:   u.KycStatus,
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
}
