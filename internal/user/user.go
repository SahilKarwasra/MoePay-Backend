package user

import (
	"time"

	"github.com/google/uuid"
)

const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
	StatusDeleted   = "deleted"
)

type User struct {
	ID          uuid.UUID
	PhoneE164   string
	Status      string
	Username    *string
	KYCStatus   string
	LastLoginAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type Profile struct {
	FirstName   *string
	LastName    *string
	Email       *string
	DateOfBirth *time.Time
	AvatarURL   *string
	UpdatedAt   *time.Time
}

// ProfileView is the client representation of a profile.
type ProfileView struct {
	FirstName   *string `json:"first_name"`
	LastName    *string `json:"last_name"`
	Email       *string `json:"email"`
	DateOfBirth *string `json:"date_of_birth"`
	AvatarURL   *string `json:"avatar_url"`
}

func (p Profile) View() ProfileView {
	v := ProfileView{FirstName: p.FirstName, LastName: p.LastName, Email: p.Email, AvatarURL: p.AvatarURL}
	if p.DateOfBirth != nil {
		s := p.DateOfBirth.Format(time.DateOnly)
		v.DateOfBirth = &s
	}
	return v
}

// View is the client representation of the current user.
type View struct {
	ID          string      `json:"id"`
	PhoneNumber string      `json:"phone_number"`
	Username    *string     `json:"username"`
	KYCStatus   string      `json:"kyc_status"`
	Status      string      `json:"status"`
	Profile     ProfileView `json:"profile"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

func NewView(u User, p Profile) View {
	return View{
		ID:          u.ID.String(),
		PhoneNumber: u.PhoneE164,
		Username:    u.Username,
		KYCStatus:   u.KYCStatus,
		Status:      u.Status,
		Profile:     p.View(),
		CreatedAt:   u.CreatedAt,
		UpdatedAt:   u.UpdatedAt,
	}
}
