package controllers

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/sahilkarwasra/moepay/internal/middleware"
	"github.com/sahilkarwasra/moepay/internal/models"
	"github.com/sahilkarwasra/moepay/internal/repository"
	"github.com/sahilkarwasra/moepay/internal/service"
	"github.com/sahilkarwasra/moepay/internal/utils"
)

type UserHandler struct {
	userService *service.UserService
}

func NewUserHandler(userService *service.UserService) *UserHandler {
	return &UserHandler{
		userService: userService,
	}
}

func (h *UserHandler) UpdateUser(c *gin.Context) {
	// Extract user ID from authenticated context or URL param
	userID := c.GetString(middleware.ContextUserIDKey)
	if userID == "" {
		userID = c.Param("id")
	}

	if userID == "" {
		utils.BadRequest(c, "User ID is required")
		return
	}

	var req models.UpdateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		utils.BadRequest(c, "Invalid request payload: "+err.Error())
		return
	}

	ctx := c.Request.Context()
	updatedUser, err := h.userService.UpdateUser(ctx, userID, req)

	if err != nil {
		switch {
		case errors.Is(err, repository.ErrUserNotFound):
			utils.NotFound(c, "User not found")
		case errors.Is(err, repository.ErrWalletIdAlreadyExists):
			utils.Conflict(c, "Wallet ID is already in use by another user")
		case errors.Is(err, repository.ErrInvalidUserID):
			utils.BadRequest(c, "Invalid user ID format")
		case errors.Is(err, repository.ErrInvalidUserName):
			utils.BadRequest(c, "User name must be between 3 and 30 characters")
		case errors.Is(err, repository.ErrInvalidWalletID):
			utils.BadRequest(c, "Wallet ID must be between 3 and 50 characters")
		default:
			utils.InternalServerError(c, "Failed to update user: "+err.Error())
		}
		return
	}

	utils.Success(c, "User updated successfully", http.StatusOK, updatedUser.ToPublic())
}
