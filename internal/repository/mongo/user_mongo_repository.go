package mongo

import (
	"context"
	"errors"

	"github.com/sahilkarwasra/moepay/internal/models"
	"github.com/sahilkarwasra/moepay/internal/repository"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDriver "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type UserMongoRepository struct {
	collection *mongoDriver.Collection
}

func NewUserMongoRepository(
	collection *mongoDriver.Collection,
) repository.UserRepository {
	return &UserMongoRepository{
		collection: collection,
	}
}

func (r *UserMongoRepository) EnsureIndexes(ctx context.Context) error {
	indexModel := mongoDriver.IndexModel{
		Keys:    bson.D{{Key: "wallet_id", Value: 1}},
		Options: options.Index().SetUnique(true).SetSparse(true),
	}
	_, err := r.collection.Indexes().CreateOne(ctx, indexModel)
	return err
}

func (r *UserMongoRepository) CreateUser(
	ctx context.Context,
	user *models.User,
) error {
	_, err := r.collection.InsertOne(ctx, user)
	if err != nil {
		if mongoDriver.IsDuplicateKeyError(err) {
			return repository.ErrUserAlreadyExists
		}
		return err
	}

	return nil
}

func (r *UserMongoRepository) GetUserByEmail(
	ctx context.Context,
	email string,
) (*models.User, error) {
	var user models.User

	err := r.collection.
		FindOne(ctx, bson.M{"email": email}).
		Decode(&user)

	if err != nil {
		if errors.Is(err, mongoDriver.ErrNoDocuments) {
			return nil, repository.ErrUserNotFound
		}
		return nil, err
	}

	return &user, nil
}

func (r *UserMongoRepository) GetUserByID(
	ctx context.Context,
	userID string,
) (*models.User, error) {
	objectID, err := primitive.ObjectIDFromHex(userID)
	if err != nil {
		return nil, repository.ErrInvalidUserID
	}

	var user models.User

	err = r.collection.
		FindOne(ctx, bson.M{"_id": objectID}).
		Decode(&user)

	if err != nil {
		if errors.Is(err, mongoDriver.ErrNoDocuments) {
			return nil, repository.ErrUserNotFound
		}
		return nil, err
	}

	return &user, nil
}

func (r *UserMongoRepository) GetUserByWalletID(
	ctx context.Context,
	walletID string,
) (*models.User, error) {
	var user models.User

	err := r.collection.
		FindOne(ctx, bson.M{"wallet_id": walletID}).
		Decode(&user)

	if err != nil {
		if errors.Is(err, mongoDriver.ErrNoDocuments) {
			return nil, repository.ErrUserNotFound
		}
		return nil, err
	}

	return &user, nil
}

func (r *UserMongoRepository) UpdateUser(
	ctx context.Context,
	user *models.User,
) error {
	filter := bson.M{
		"_id": user.ID,
	}

	update := bson.M{
		"$set": bson.M{
			"user_name":  user.UserName,
			"wallet_id":  user.WalletId,
			"updated_at": user.UpdatedAt,
		},
	}

	result, err := r.collection.UpdateOne(ctx, filter, update)
	if err != nil {
		if mongoDriver.IsDuplicateKeyError(err) {
			return repository.ErrWalletIdAlreadyExists
		}
		return err
	}

	if result.MatchedCount == 0 {
		return repository.ErrUserNotFound
	}

	return nil
}

func (r *UserMongoRepository) DeleteUser(
	ctx context.Context,
	userID string,
) error {
	objectID, err := primitive.ObjectIDFromHex(userID)
	if err != nil {
		return err
	}

	result, err := r.collection.DeleteOne(
		ctx,
		bson.M{"_id": objectID},
	)
	if err != nil {
		return err
	}

	if result.DeletedCount == 0 {
		return repository.ErrUserNotFound
	}

	return nil
}

func (r *UserMongoRepository) GetUserByPhoneNumber(
	ctx context.Context,
	phoneNumber string,
) (*models.User, error) {
	var user models.User

	err := r.collection.
		FindOne(ctx, bson.M{"phone_number": phoneNumber}).
		Decode(&user)

	if err != nil {
		if errors.Is(err, mongoDriver.ErrNoDocuments) {
			return nil, repository.ErrUserNotFound
		}
		return nil, err
	}

	return &user, nil
}
