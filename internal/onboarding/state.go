package onboarding

import (
	"slices"
	"time"

	"github.com/sahilkarwasra/moepay/internal/user"
)

type Step string

const (
	StepProfile  Step = "profile"
	StepUsername Step = "username"
	// StepReview means every required step is satisfied and the client must call
	// POST /onboarding/complete.
	StepReview    Step = "review"
	StepCompleted Step = "completed"
)

const (
	StatusInProgress = "in_progress"
	StatusCompleted  = "completed"
)

// RequiredSteps is the ordered list of steps; a step may only be saved once every
// step before it is satisfied.
var RequiredSteps = []Step{StepProfile, StepUsername}

// Progress is the persisted onboarding_progress row.
type Progress struct {
	Status         string
	CurrentStep    Step
	CompletedSteps []Step
	CompletedAt    *time.Time
	Version        int
	UpdatedAt      time.Time
}

// satisfied reports which steps the user's saved data satisfies.
func satisfied(u user.User, p user.Profile) map[Step]bool {
	return map[Step]bool{
		StepProfile:  p.FirstName != nil && p.LastName != nil,
		StepUsername: u.Username != nil,
	}
}

// derive computes completed steps and the next step from saved data.
func derive(u user.User, p user.Profile, completed bool) (done []Step, next Step) {
	ok := satisfied(u, p)
	for _, s := range RequiredSteps {
		if ok[s] {
			done = append(done, s)
		} else if next == "" {
			next = s
		}
	}
	switch {
	case completed:
		next = StepCompleted
	case next == "":
		next = StepReview
	}
	if done == nil {
		done = []Step{}
	}
	return done, next
}

// canSave enforces step ordering: every step before target must be satisfied.
func canSave(target Step, u user.User, p user.Profile) bool {
	ok := satisfied(u, p)
	idx := slices.Index(RequiredSteps, target)
	for _, s := range RequiredSteps[:idx] {
		if !ok[s] {
			return false
		}
	}
	return true
}

// StepView describes one step for the client.
type StepView struct {
	Step      Step `json:"step"`
	Completed bool `json:"completed"`
}

// State is the API representation returned by every onboarding endpoint.
type State struct {
	Status         string     `json:"status"`
	NextStep       Step       `json:"next_step"`
	CompletedSteps []Step     `json:"completed_steps"`
	Steps          []StepView `json:"steps"`
	Data           StateData  `json:"data"`
	CompletedAt    *time.Time `json:"completed_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type StateData struct {
	PhoneNumber string           `json:"phone_number"`
	Username    *string          `json:"username"`
	Profile     user.ProfileView `json:"profile"`
}

// Summary is the compact form embedded in auth responses.
type Summary struct {
	Status   string `json:"status"`
	NextStep Step   `json:"next_step"`
}

func buildState(u user.User, p user.Profile, prog Progress) State {
	done, next := derive(u, p, prog.Status == StatusCompleted)
	steps := make([]StepView, len(RequiredSteps))
	for i, s := range RequiredSteps {
		steps[i] = StepView{Step: s, Completed: slices.Contains(done, s)}
	}
	return State{
		Status:         prog.Status,
		NextStep:       next,
		CompletedSteps: done,
		Steps:          steps,
		Data: StateData{
			PhoneNumber: u.PhoneE164,
			Username:    u.Username,
			Profile:     p.View(),
		},
		CompletedAt: prog.CompletedAt,
		UpdatedAt:   prog.UpdatedAt,
	}
}
