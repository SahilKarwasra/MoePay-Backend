package controllers

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sahilkarwasra/moepay/internal/models"
	"github.com/sahilkarwasra/moepay/internal/service"
	"github.com/sahilkarwasra/moepay/internal/utils"
)

type AuthHandler struct {
	userService *service.UserService
}

func NewAuthHandler(userService *service.UserService) *AuthHandler {
	return &AuthHandler{
		userService: userService,
	}
}

func (h *AuthHandler) SendOTP(c *gin.Context) {

	var req models.SendOtpRequest

	if err := c.ShouldBindBodyWithJSON(&req); err != nil {
		utils.BadRequest(c, "Invalid request body")
		return
	}

	ctx := c.Request.Context()

	_, err := h.userService.SendOTP(ctx, req.PhoneNumber)

	if err != nil {
		utils.InternalServerError(c, "Failed to send OTP")
		return
	}

	utils.Success(c, "OTP Sent Successfully", http.StatusOK, gin.H{
		"phone_number": req.PhoneNumber,
		"expires_in":   "10 Minutes",
	})

}

func (h *AuthHandler) VerifyOTP(c *gin.Context) {
	var req models.VerifyOtpRequest

	if err := c.ShouldBindBodyWithJSON(&req); err != nil {
		utils.BadRequest(c, "Invalid request body")
		return
	}

	ctx := c.Request.Context()

	result, err := h.userService.VerifyOtpRequest(ctx, req.PhoneNumber, req.Otp)

	if err != nil {
		utils.InternalServerError(c, err.Error())
		return
	}

	utils.Success(c, "OTP Verified Successfully", http.StatusOK, gin.H{
		"access_token":  result.AccessToken,
		"refresh_token": result.RefreshToken,
		"is_user_new":   result.IsUserNew,
	})
}
