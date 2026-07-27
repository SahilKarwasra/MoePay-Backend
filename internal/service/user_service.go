package service

import (
	"context"
	"strings"
	"time"

	"github.com/sahilkarwasra/moepay/internal/models"
	"github.com/sahilkarwasra/moepay/internal/repository"
	"github.com/sahilkarwasra/moepay/internal/utils"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

type UserService struct {
	userRepo  repository.UserRepository
	otpRepo   repository.OtpRepository
	tokenRepo repository.TokenRepository
}

func NewUserService(
	userRepo repository.UserRepository,
	otpRepo repository.OtpRepository,
	tokenRepo repository.TokenRepository,
) *UserService {
	return &UserService{
		userRepo:  userRepo,
		otpRepo:   otpRepo,
		tokenRepo: tokenRepo,
	}
}

func (s *UserService) SendOTP(ctx context.Context, phoneNumber string) (string, error) {
	if len(phoneNumber) < 8 {
		return "", repository.ErrInvalidPhoneNumber
	}

	// delete old otp of this number
	err := s.otpRepo.DeleteManyByPhoneNumber(ctx, phoneNumber)
	if err != nil {
		return "", err
	}

	// generate otp
	otp := utils.GenerateOTP()
	otpDoc := models.Otp{
		ID:          primitive.NewObjectID(),
		PhoneNumber: phoneNumber,
		Otp:         otp,
		IsUsed:      false,
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(10 * time.Minute),
	}

	err = s.otpRepo.InsertOne(ctx, &otpDoc)
	if err != nil {
		return "", err
	}

	utils.LogOTP(phoneNumber, otp)

	return otp, nil
}

func (s *UserService) VerifyOtpRequest(ctx context.Context, phoneNumber string, otp string) (models.VerifyOTPResponse, error) {
	var isUserNew = false

	// basic validation
	if len(phoneNumber) < 8 {
		return models.VerifyOTPResponse{}, repository.ErrInvalidPhoneNumber
	}
	if len(otp) != 6 {
		return models.VerifyOTPResponse{}, repository.ErrInvalidOtp
	}

	// find otp
	otpDoc, err := s.otpRepo.FindOtpByPhoneNumber(ctx, phoneNumber)

	// checking for error and not found
	if err != nil {
		return models.VerifyOTPResponse{}, repository.ErrOtpNotFound
	}

	// checking if otp matches
	if otpDoc.Otp != otp {
		return models.VerifyOTPResponse{}, repository.ErrInvalidOtp
	}

	// checking if otp is used or not
	if otpDoc.IsUsed {
		return models.VerifyOTPResponse{}, repository.ErrOtpUsed
	}

	// checking if otp is expired or not
	if otpDoc.ExpiresAt.Before(time.Now()) {
		return models.VerifyOTPResponse{}, repository.ErrOtpExpired
	}

	// mark otp as used
	otpDoc.IsUsed = true

	err = s.otpRepo.UpdateOne(ctx, otpDoc)
	if err != nil {
		return models.VerifyOTPResponse{}, err
	}

	var user *models.User
	user, err = s.userRepo.GetUserByPhoneNumber(ctx, phoneNumber)

	// check if user exists or not
	if err == repository.ErrUserNotFound {
		user = &models.User{
			ID:          primitive.NewObjectID(),
			PhoneNumber: phoneNumber,
			CreatedAt:   time.Now(),
			UpdatedAt:   time.Now(),
			UserName:    "",
			WalletId:    "",
			KycStatus:   "",
		}

		err = s.userRepo.CreateUser(ctx, user)
		if err != nil {
			return models.VerifyOTPResponse{}, err
		}
	} else if err != nil {
		return models.VerifyOTPResponse{}, err
	}

	if user.WalletId == "" {
		isUserNew = true
	}

	// generate token
	tokenPair, err := utils.GenerateTokenPair(user.ID.Hex(), user.PhoneNumber, "")
	if err != nil {
		return models.VerifyOTPResponse{}, err
	}

	// save token to db
	err = s.tokenRepo.CreateToken(ctx, &models.RefreshToken{
		ID:        primitive.NewObjectID(),
		UserId:    user.ID,
		Token:     tokenPair.RefreshToken,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(7 * 24 * time.Hour),
	})
	if err != nil {
		return models.VerifyOTPResponse{}, err
	}

	return models.VerifyOTPResponse{
		AccessToken:  tokenPair.AccessToken,
		RefreshToken: tokenPair.RefreshToken,
		IsUserNew:    isUserNew,
	}, nil
}

func (s *UserService) UpdateUser(
	ctx context.Context,
	userID string,
	req models.UpdateUserRequest,
) (*models.User, error) {
	// 1. Validate User ID
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, repository.ErrInvalidUserID
	}

	// 2. Validate input fields
	userName := strings.TrimSpace(req.UserName)
	walletID := strings.TrimSpace(req.WalletId)

	if len(userName) < 3 || len(userName) > 30 {
		return nil, repository.ErrInvalidUserName
	}

	if len(walletID) < 3 || len(walletID) > 50 {
		return nil, repository.ErrInvalidWalletID
	}

	// 3. Check if user exists
	user, err := s.userRepo.GetUserByID(ctx, userID)
	if err != nil {
		return nil, err
	}

	// 4. Verify wallet_id uniqueness in database across users
	existingUser, err := s.userRepo.GetUserByWalletID(ctx, walletID)
	if err == nil && existingUser != nil {
		// If another user already has this wallet_id
		if existingUser.ID != user.ID {
			return nil, repository.ErrWalletIdAlreadyExists
		}
	} else if err != nil && err != repository.ErrUserNotFound {
		return nil, err
	}

	// 5. Apply updates
	user.UserName = userName
	user.WalletId = walletID
	user.UpdatedAt = time.Now()

	// 6. Save in DB
	err = s.userRepo.UpdateUser(ctx, user)
	if err != nil {
		return nil, err
	}

	return user, nil
}

