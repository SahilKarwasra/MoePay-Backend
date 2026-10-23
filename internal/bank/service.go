package bank

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sahilkarwasra/moepay/internal/apperr"
	"github.com/sahilkarwasra/moepay/internal/config"
	"github.com/sahilkarwasra/moepay/internal/platform/postgres"
	"github.com/sahilkarwasra/moepay/internal/wallet"
)

type Service struct {
	pool    *pgxpool.Pool
	repo    *Repository
	wallets *wallet.Service
	cfg     config.WalletConfig
}

func NewService(pool *pgxpool.Pool, repo *Repository, wallets *wallet.Service, cfg config.WalletConfig) *Service {
	return &Service{pool: pool, repo: repo, wallets: wallets, cfg: cfg}
}

func (s *Service) List(ctx context.Context, userID uuid.UUID) ([]View, error) {
	accts, err := s.repo.List(ctx, s.pool, userID)
	if err != nil {
		return nil, err
	}
	out := make([]View, len(accts))
	for i, a := range accts {
		out[i] = a.View()
	}
	return out, nil
}

type AddInput struct {
	HolderName    string `json:"holder_name"`
	SortCode      string `json:"sort_code"`
	AccountNumber string `json:"account_number"`
	MakePrimary   bool   `json:"make_primary"`
}

// Add links a bank account. The first account becomes primary automatically.
func (s *Service) Add(ctx context.Context, userID uuid.UUID, in AddInput) (View, error) {
	if _, err := s.wallets.ForUser(ctx, s.pool, userID); err != nil {
		return View{}, err
	}
	fields := map[string]string{}
	holder, ok := cleanHolderName(in.HolderName)
	if !ok {
		fields["holder_name"] = "required, up to 70 letters, spaces or . - ' &"
	}
	sortCode, ok := NormalizeSortCode(in.SortCode)
	if !ok {
		fields["sort_code"] = "must be 6 digits, e.g. 04-00-04"
	}
	number, ok := normalizeAccountNumber(in.AccountNumber)
	if !ok {
		fields["account_number"] = "must be 8 digits"
	}
	if len(fields) > 0 {
		return View{}, validationErr(fields)
	}

	var out Account
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.repo.lockUser(ctx, tx, userID); err != nil {
			return err
		}
		n, err := s.repo.countActive(ctx, tx, userID)
		if err != nil {
			return err
		}
		if n >= s.cfg.MaxBankAccounts {
			return apperr.ErrBankAccountLimit.WithDetails(map[string]any{"max": s.cfg.MaxBankAccounts})
		}
		primary := n == 0 || in.MakePrimary
		if primary && n > 0 {
			if err := s.repo.clearPrimary(ctx, tx, userID); err != nil {
				return err
			}
		}
		out, err = s.repo.insert(ctx, tx, Account{
			UserID: userID, HolderName: holder, BankName: BankName(sortCode),
			SortCode: sortCode, Last4: number[len(number)-4:], IsPrimary: primary,
		}, accountHash(s.cfg.AccountHashSecret, sortCode, number))
		return err
	})
	if err != nil {
		return View{}, err
	}
	return out.View(), nil
}

func (s *Service) SetPrimary(ctx context.Context, userID, id uuid.UUID) (View, error) {
	var out Account
	err := postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.repo.lockUser(ctx, tx, userID); err != nil {
			return err
		}
		a, err := s.repo.Get(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		if !a.IsPrimary {
			if err := s.repo.clearPrimary(ctx, tx, userID); err != nil {
				return err
			}
			if err := s.repo.setPrimary(ctx, tx, id); err != nil {
				return err
			}
			a.IsPrimary = true
		}
		out = a
		return nil
	})
	if err != nil {
		return View{}, err
	}
	return out.View(), nil
}

// Remove unlinks an account. If it was primary, the oldest remaining one is promoted.
func (s *Service) Remove(ctx context.Context, userID, id uuid.UUID) error {
	return postgres.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := s.repo.lockUser(ctx, tx, userID); err != nil {
			return err
		}
		a, err := s.repo.Get(ctx, tx, userID, id)
		if err != nil {
			return err
		}
		if err := s.repo.remove(ctx, tx, id); err != nil {
			return err
		}
		if a.IsPrimary {
			return s.repo.promoteOldest(ctx, tx, userID)
		}
		return nil
	})
}

type SortCodeInfo struct {
	SortCode string `json:"sort_code"`
	BankName string `json:"bank_name"`
}

func (s *Service) LookupSortCode(raw string) (SortCodeInfo, error) {
	sc, ok := NormalizeSortCode(raw)
	if !ok {
		return SortCodeInfo{}, validationErr(map[string]string{"sort_code": "must be 6 digits, e.g. 04-00-04"})
	}
	return SortCodeInfo{SortCode: FormatSortCode(sc), BankName: BankName(sc)}, nil
}
