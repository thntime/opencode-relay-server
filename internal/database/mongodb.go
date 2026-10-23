package database

import (
	"context"
	"database/sql"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"github.com/zero469/opencode-relay-server/internal/models"
)

type MongoDB struct {
	client            *mongo.Client
	db                *mongo.Database
	users             *mongo.Collection
	devices           *mongo.Collection
	verificationCodes *mongo.Collection
	pairingRequests   *mongo.Collection
	counters          *mongo.Collection
}

func NewMongoDB(uri string) (*MongoDB, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	clientOpts := options.Client().ApplyURI(uri)
	client, err := mongo.Connect(ctx, clientOpts)
	if err != nil {
		return nil, err
	}

	if err := client.Ping(ctx, nil); err != nil {
		_ = client.Disconnect(ctx)
		return nil, err
	}

	db := client.Database("opencode_relay")

	mdb := &MongoDB{
		client:            client,
		db:                db,
		users:             db.Collection("users"),
		devices:           db.Collection("devices"),
		verificationCodes: db.Collection("verification_codes"),
		pairingRequests:   db.Collection("pairing_requests"),
		counters:          db.Collection("counters"),
	}

	if err := mdb.createIndexes(ctx); err != nil {
		_ = client.Disconnect(ctx)
		return nil, err
	}

	return mdb, nil
}

func (m *MongoDB) createIndexes(ctx context.Context) error {
	_, err := m.users.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "email", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return err
	}

	_, err = m.devices.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "subdomain", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return err
	}

	_, err = m.devices.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "user_id", Value: 1}},
	})
	if err != nil {
		return err
	}

	_, err = m.verificationCodes.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "email", Value: 1}},
	})
	if err != nil {
		return err
	}

	_, err = m.pairingRequests.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "user_id", Value: 1}},
	})
	return err
}

func (m *MongoDB) getNextID(ctx context.Context, collection string) (int64, error) {
	filter := bson.M{"_id": collection}
	update := bson.M{"$inc": bson.M{"seq": int64(1)}}
	opts := options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)

	var result struct {
		Seq int64 `bson:"seq"`
	}
	err := m.counters.FindOneAndUpdate(ctx, filter, update, opts).Decode(&result)
	if err != nil {
		return 0, err
	}
	return result.Seq, nil
}

func (m *MongoDB) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return m.client.Disconnect(ctx)
}

func (m *MongoDB) CreateUser(email, passwordHash string) (*models.User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id, err := m.getNextID(ctx, "users")
	if err != nil {
		return nil, err
	}

	now := time.Now()
	user := &models.User{
		ID:           id,
		Email:        email,
		PasswordHash: passwordHash,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	_, err = m.users.InsertOne(ctx, user)
	if err != nil {
		return nil, err
	}

	return user, nil
}

func (m *MongoDB) GetUserByID(id int64) (*models.User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var user models.User
	err := m.users.FindOne(ctx, bson.M{"id": id}).Decode(&user)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return &user, nil
}

func (m *MongoDB) GetUserByEmail(email string) (*models.User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var user models.User
	err := m.users.FindOne(ctx, bson.M{"email": email}).Decode(&user)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return &user, nil
}

func (m *MongoDB) ListUsers() ([]*models.User, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cursor, err := m.users.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var users []*models.User
	if err := cursor.All(ctx, &users); err != nil {
		return nil, err
	}
	return users, nil
}

func (m *MongoDB) UpdateUserEmail(id int64, email string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := m.users.UpdateOne(ctx, bson.M{"id": id}, bson.M{"$set": bson.M{
		"email":      email,
		"updated_at": time.Now(),
	}})
	if err != nil {
		return err
	}
	if result.MatchedCount == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (m *MongoDB) UpdateUserPassword(id int64, passwordHash string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := m.users.UpdateOne(ctx, bson.M{"id": id}, bson.M{"$set": bson.M{
		"password_hash": passwordHash,
		"updated_at":    time.Now(),
	}})
	if err != nil {
		return err
	}
	if result.MatchedCount == 0 {
		return ErrUserNotFound
	}
	return nil
}

func (m *MongoDB) CreateDevice(userID int64, name, subdomain, authUser, authPassword string) (*models.Device, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id, err := m.getNextID(ctx, "devices")
	if err != nil {
		return nil, err
	}

	now := time.Now()
	device := &models.Device{
		ID:           id,
		UserID:       userID,
		Name:         name,
		Subdomain:    subdomain,
		AuthUser:     authUser,
		AuthPassword: authPassword,
		Online:       false,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	_, err = m.devices.InsertOne(ctx, device)
	if err != nil {
		return nil, err
	}

	return device, nil
}

func (m *MongoDB) GetDeviceByID(id int64) (*models.Device, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var device models.Device
	err := m.devices.FindOne(ctx, bson.M{"id": id}).Decode(&device)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return &device, nil
}

func (m *MongoDB) GetDeviceBySubdomain(subdomain string) (*models.Device, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var device models.Device
	err := m.devices.FindOne(ctx, bson.M{"subdomain": subdomain}).Decode(&device)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return &device, nil
}

func (m *MongoDB) GetDevicesByUserID(userID int64) ([]*models.Device, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cursor, err := m.devices.Find(ctx, bson.M{"user_id": userID})
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)

	var devices []*models.Device
	if err := cursor.All(ctx, &devices); err != nil {
		return nil, err
	}

	if devices == nil {
		devices = []*models.Device{}
	}

	return devices, nil
}

func (m *MongoDB) UpdateDevice(id int64, name string) (*models.Device, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	update := bson.M{
		"$set": bson.M{
			"name":       name,
			"updated_at": time.Now(),
		},
	}

	_, err := m.devices.UpdateOne(ctx, bson.M{"id": id}, update)
	if err != nil {
		return nil, err
	}

	return m.GetDeviceByID(id)
}

func (m *MongoDB) DeleteDevice(id int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := m.devices.DeleteOne(ctx, bson.M{"id": id})
	return err
}

func (m *MongoDB) UpdateDeviceHeartbeat(id int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	now := time.Now()
	update := bson.M{
		"$set": bson.M{
			"online":     true,
			"last_seen":  now,
			"updated_at": now,
		},
	}

	_, err := m.devices.UpdateOne(ctx, bson.M{"id": id}, update)
	return err
}

func (m *MongoDB) MarkOfflineDevices(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cutoff := time.Now().Add(-timeout)
	filter := bson.M{
		"online":    true,
		"last_seen": bson.M{"$lt": cutoff},
	}
	update := bson.M{
		"$set": bson.M{"online": false},
	}

	_, err := m.devices.UpdateMany(ctx, filter, update)
	return err
}

func (m *MongoDB) MarkDeviceOffline(id int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	update := bson.M{
		"$set": bson.M{
			"online":     false,
			"updated_at": time.Now(),
		},
	}

	_, err := m.devices.UpdateOne(ctx, bson.M{"id": id}, update)
	return err
}

func (m *MongoDB) MarkAllDevicesOffline() (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := m.devices.UpdateMany(ctx,
		bson.M{"online": true},
		bson.M{"$set": bson.M{"online": false}},
	)
	if err != nil {
		return 0, err
	}
	return result.ModifiedCount, nil
}

func (m *MongoDB) SetDeviceKeyMaterial(deviceID int64, keyWrapped, keySalt, keyKdf string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	update := bson.M{
		"$set": bson.M{
			"key_wrapped": keyWrapped,
			"key_salt":    keySalt,
			"key_kdf":     keyKdf,
			"updated_at":  time.Now(),
		},
	}

	_, err := m.devices.UpdateOne(ctx, bson.M{"id": deviceID}, update)
	return err
}

func (m *MongoDB) GetDeviceKeyMaterial(deviceID int64) (string, string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var doc struct {
		KeyWrapped string `bson:"key_wrapped"`
		KeySalt    string `bson:"key_salt"`
		KeyKdf     string `bson:"key_kdf"`
	}
	opts := options.FindOne().SetProjection(bson.M{
		"key_wrapped": 1,
		"key_salt":    1,
		"key_kdf":     1,
	})
	err := m.devices.FindOne(ctx, bson.M{"id": deviceID}, opts).Decode(&doc)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return "", "", "", sql.ErrNoRows
		}
		return "", "", "", err
	}
	return doc.KeyWrapped, doc.KeySalt, doc.KeyKdf, nil
}

func (m *MongoDB) CreateVerificationCode(email, code string, expiresAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id, err := m.getNextID(ctx, "verification_codes")
	if err != nil {
		return err
	}

	doc := models.VerificationCode{
		ID:        id,
		Email:     email,
		Code:      code,
		ExpiresAt: expiresAt,
		Used:      false,
		CreatedAt: time.Now(),
	}

	_, err = m.verificationCodes.InsertOne(ctx, doc)
	return err
}

func (m *MongoDB) GetValidVerificationCode(email, code string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	filter := bson.M{
		"email":      email,
		"code":       code,
		"expires_at": bson.M{"$gt": time.Now()},
		"used":       false,
	}

	count, err := m.verificationCodes.CountDocuments(ctx, filter)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (m *MongoDB) MarkVerificationCodeUsed(email, code string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	filter := bson.M{"email": email, "code": code}
	update := bson.M{"$set": bson.M{"used": true}}

	_, err := m.verificationCodes.UpdateMany(ctx, filter, update)
	return err
}

func (m *MongoDB) DeleteExpiredVerificationCodes() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := m.verificationCodes.DeleteMany(ctx, bson.M{
		"expires_at": bson.M{"$lt": time.Now()},
	})
	return err
}

func (m *MongoDB) CreatePairingRequest(id string, userID int64, pairingCode string, expiresAt time.Time) (*models.PairingRequest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	now := time.Now()
	pr := &models.PairingRequest{
		ID:          id,
		UserID:      userID,
		PairingCode: pairingCode,
		Status:      "pending",
		ExpiresAt:   expiresAt,
		CreatedAt:   now,
	}

	_, err := m.pairingRequests.InsertOne(ctx, pr)
	if err != nil {
		return nil, err
	}
	return pr, nil
}

func (m *MongoDB) GetPairingRequestByID(id string) (*models.PairingRequest, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var pr models.PairingRequest
	err := m.pairingRequests.FindOne(ctx, bson.M{"id": id}).Decode(&pr)
	if err != nil {
		if err == mongo.ErrNoDocuments {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return &pr, nil
}

func (m *MongoDB) CompletePairingRequest(id string, deviceID int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	update := bson.M{
		"$set": bson.M{
			"status":    "completed",
			"device_id": deviceID,
		},
	}

	_, err := m.pairingRequests.UpdateOne(ctx, bson.M{"id": id}, update)
	return err
}

func (m *MongoDB) DeleteExpiredPairingRequests() (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := m.pairingRequests.DeleteMany(ctx, bson.M{
		"expires_at": bson.M{"$lt": time.Now()},
		"status":     "pending",
	})
	if err != nil {
		return 0, err
	}
	return result.DeletedCount, nil
}
