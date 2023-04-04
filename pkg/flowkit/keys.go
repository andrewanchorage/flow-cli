/*
 * Flow CLI
 *
 * Copyright 2019 Dapper Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package flowkit

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	goeth "github.com/ethereum/go-ethereum/accounts"
	"github.com/lmars/go-slip10"
	"github.com/onflow/flow-go-sdk/crypto"
	"github.com/onflow/flow-go-sdk/crypto/cloudkms"
	"github.com/tyler-smith/go-bip39"

	"github.com/onflow/flow-cli/pkg/flowkit/config"
)

// AccountKey is a flowkit specific account key implementation
// allowing us to sign the transactions using different implemented methods.
type AccountKey interface {
	Type() config.KeyType
	Index() int
	SigAlgo() crypto.SignatureAlgorithm
	HashAlgo() crypto.HashAlgorithm
	Signer(ctx context.Context) (crypto.Signer, error)
	ToConfig() config.AccountKey
	Validate() error
	PrivateKey() (*crypto.PrivateKey, error)
}

var _ AccountKey = &HexAccountKey{}

var _ AccountKey = &KmsAccountKey{}

var _ AccountKey = &Bip44AccountKey{}

func accountKeyFromConfig(accountKeyConf config.AccountKey) (AccountKey, error) {
	switch accountKeyConf.Type {
	case config.KeyTypeHex:
		return hexKeyFromConfig(accountKeyConf)
	case config.KeyTypeBip44:
		return bip44KeyFromConfig(accountKeyConf)
	case config.KeyTypeGoogleKMS:
		return kmsKeyFromConfig(accountKeyConf)
	case config.KeyTypeFile:
		return fileKeyFromConfig(accountKeyConf)
	}

	return nil, fmt.Errorf(`invalid key type: "%s"`, accountKeyConf.Type)
}

type baseAccountKey struct {
	keyType  config.KeyType
	index    int
	sigAlgo  crypto.SignatureAlgorithm
	hashAlgo crypto.HashAlgorithm
}

func baseKeyFromConfig(accountKeyConf config.AccountKey) *baseAccountKey {
	return &baseAccountKey{
		keyType:  accountKeyConf.Type,
		index:    accountKeyConf.Index,
		sigAlgo:  accountKeyConf.SigAlgo,
		hashAlgo: accountKeyConf.HashAlgo,
	}
}

func (a *baseAccountKey) Type() config.KeyType {
	return a.keyType
}

func (a *baseAccountKey) SigAlgo() crypto.SignatureAlgorithm {
	if a.sigAlgo == crypto.UnknownSignatureAlgorithm {
		return crypto.ECDSA_P256 // default value
	}
	return a.sigAlgo
}

func (a *baseAccountKey) HashAlgo() crypto.HashAlgorithm {
	if a.hashAlgo == crypto.UnknownHashAlgorithm {
		return crypto.SHA3_256 // default value
	}
	return a.hashAlgo
}

func (a *baseAccountKey) Index() int {
	return a.index // default to 0
}

func (a *baseAccountKey) Validate() error {
	return nil
}

// KmsAccountKey implements Gcloud KMS system for signing.
type KmsAccountKey struct {
	*baseAccountKey
	kmsKey cloudkms.Key
}

// ToConfig convert account key to configuration.
func (a *KmsAccountKey) ToConfig() config.AccountKey {
	return config.AccountKey{
		Type:       a.keyType,
		Index:      a.index,
		SigAlgo:    a.sigAlgo,
		HashAlgo:   a.hashAlgo,
		ResourceID: a.kmsKey.ResourceID(),
	}
}

func (a *KmsAccountKey) Signer(ctx context.Context) (crypto.Signer, error) {
	kmsClient, err := cloudkms.NewClient(ctx)
	if err != nil {
		return nil, err
	}

	accountKMSSigner, err := kmsClient.SignerForKey(
		ctx,
		a.kmsKey,
	)
	if err != nil {
		return nil, err
	}

	return accountKMSSigner, nil
}

func (a *KmsAccountKey) Validate() error {
	return gcloudApplicationSignin(a.kmsKey.ResourceID())
}

func (a *KmsAccountKey) PrivateKey() (*crypto.PrivateKey, error) {
	return nil, fmt.Errorf("private key not accessible")
}

// gcloudApplicationSignin signs in as an application user using gcloud command line tool
// currently assumes gcloud is already installed on the machine
// will by default pop a browser window to sign in
func gcloudApplicationSignin(resourceID string) error {
	googleAppCreds := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if len(googleAppCreds) > 0 {
		return nil
	}

	kms, err := cloudkms.KeyFromResourceID(resourceID)
	if err != nil {
		return err
	}

	proj := kms.ProjectID
	if len(proj) == 0 {
		return fmt.Errorf(
			"could not get GOOGLE_APPLICATION_CREDENTIALS, no google service account JSON provided but private key type is KMS",
		)
	}

	loginCmd := exec.Command("gcloud", "auth", "application-default", "login", fmt.Sprintf("--project=%s", proj))

	output, err := loginCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to run %q: %s\n", loginCmd.String(), err)
	}

	squareBracketRegex := regexp.MustCompile(`(?s)\[(.*)\]`)
	regexResult := squareBracketRegex.FindAllStringSubmatch(string(output), -1)
	// Should only be one value. Second index since first index contains the square brackets
	googleApplicationCreds := regexResult[0][1]

	os.Setenv("GOOGLE_APPLICATION_CREDENTIALS", googleApplicationCreds)

	return nil
}

func kmsKeyFromConfig(key config.AccountKey) (AccountKey, error) {
	accountKMSKey, err := cloudkms.KeyFromResourceID(key.ResourceID)
	if err != nil {
		return nil, err
	}

	return &KmsAccountKey{
		baseAccountKey: &baseAccountKey{
			keyType:  config.KeyTypeGoogleKMS,
			index:    key.Index,
			sigAlgo:  key.SigAlgo,
			hashAlgo: key.HashAlgo,
		},
		kmsKey: accountKMSKey,
	}, nil
}

// HexAccountKey implements account key in hex representation.
type HexAccountKey struct {
	*baseAccountKey
	privateKey crypto.PrivateKey
}

func NewHexAccountKeyFromPrivateKey(
	index int,
	hashAlgo crypto.HashAlgorithm,
	privateKey crypto.PrivateKey,
) *HexAccountKey {
	return &HexAccountKey{
		baseAccountKey: &baseAccountKey{
			keyType:  config.KeyTypeHex,
			index:    index,
			sigAlgo:  privateKey.Algorithm(),
			hashAlgo: hashAlgo,
		},
		privateKey: privateKey,
	}
}

func hexKeyFromConfig(accountKey config.AccountKey) (*HexAccountKey, error) {
	return &HexAccountKey{
		baseAccountKey: baseKeyFromConfig(accountKey),
		privateKey:     accountKey.PrivateKey,
	}, nil
}

func (a *HexAccountKey) Signer(ctx context.Context) (crypto.Signer, error) {
	return crypto.NewInMemorySigner(a.privateKey, a.HashAlgo())
}

func (a *HexAccountKey) PrivateKey() (*crypto.PrivateKey, error) {
	return &a.privateKey, nil
}

func (a *HexAccountKey) ToConfig() config.AccountKey {
	return config.AccountKey{
		Type:       a.keyType,
		Index:      a.index,
		SigAlgo:    a.sigAlgo,
		HashAlgo:   a.hashAlgo,
		PrivateKey: a.privateKey,
	}
}

func (a *HexAccountKey) Validate() error {
	_, err := crypto.DecodePrivateKeyHex(a.sigAlgo, a.PrivateKeyHex())
	if err != nil {
		return fmt.Errorf("invalid private key: %w", err)
	}

	return nil
}

func (a *HexAccountKey) PrivateKeyHex() string {
	return hex.EncodeToString(a.privateKey.Encode())
}

// fileKeyFromConfig creates a hex account key from a file location
func fileKeyFromConfig(accountKey config.AccountKey) (*FileAccountKey, error) {
	return &FileAccountKey{
		baseAccountKey: baseKeyFromConfig(accountKey),
		location:       accountKey.Location,
	}, nil
}

// NewFileAccountKey creates a new account key that is stored to a separate file in the provided location.
//
// This type of the key is a more secure way of storing accounts. The config only includes the location and not the key.
func NewFileAccountKey(
	location string,
	index int,
	sigAlgo crypto.SignatureAlgorithm,
	hashAlgo crypto.HashAlgorithm,
) *FileAccountKey {
	return &FileAccountKey{
		baseAccountKey: &baseAccountKey{
			keyType:  config.KeyTypeFile,
			index:    index,
			sigAlgo:  sigAlgo,
			hashAlgo: hashAlgo,
		},
		location: location,
	}
}

type FileAccountKey struct {
	*baseAccountKey
	privateKey crypto.PrivateKey
	location   string
}

func (f *FileAccountKey) Signer(ctx context.Context) (crypto.Signer, error) {
	key, err := f.PrivateKey()
	if err != nil {
		return nil, err
	}

	return crypto.NewInMemorySigner(*key, f.HashAlgo())
}

func (f *FileAccountKey) PrivateKey() (*crypto.PrivateKey, error) {
	if f.privateKey == nil { // lazy load the key
		key, err := os.ReadFile(f.location) // TODO(sideninja) change to use the state ReaderWriter
		if err != nil {
			return nil, fmt.Errorf("could not load the key for the account from provided location %s: %w", f.location, err)
		}
		pkey, err := crypto.DecodePrivateKeyHex(f.sigAlgo, strings.TrimPrefix(string(key), "0x"))
		if err != nil {
			return nil, fmt.Errorf("could not decode the key from provided location %s: %w", f.location, err)
		}
		f.privateKey = pkey
	}
	return &f.privateKey, nil
}

func (f *FileAccountKey) ToConfig() config.AccountKey {
	return config.AccountKey{
		Type:     config.KeyTypeFile,
		SigAlgo:  f.sigAlgo,
		HashAlgo: f.hashAlgo,
		Location: f.location,
	}
}

// Bip44AccountKey implements https://github.com/onflow/flow/blob/master/flips/20201125-bip-44-multi-account.md
type Bip44AccountKey struct {
	*baseAccountKey
	privateKey     crypto.PrivateKey
	mnemonic       string
	derivationPath string
}

func bip44KeyFromConfig(key config.AccountKey) (AccountKey, error) {
	return &Bip44AccountKey{
		baseAccountKey: &baseAccountKey{
			keyType:  config.KeyTypeBip44,
			index:    key.Index,
			sigAlgo:  key.SigAlgo,
			hashAlgo: key.HashAlgo,
		},
		derivationPath: key.DerivationPath,
		mnemonic:       key.Mnemonic,
	}, nil
}

func (a *Bip44AccountKey) Signer(ctx context.Context) (crypto.Signer, error) {
	pkey, err := a.PrivateKey()
	if err != nil {
		return nil, err
	}

	return crypto.NewInMemorySigner(*pkey, a.HashAlgo())
}

func (a *Bip44AccountKey) PrivateKey() (*crypto.PrivateKey, error) {
	if a.privateKey == nil { // lazy load
		err := a.Validate()
		if err != nil {
			return nil, err
		}
	}
	return &a.privateKey, nil
}

func (a *Bip44AccountKey) ToConfig() config.AccountKey {
	return config.AccountKey{
		Type:           a.keyType,
		Index:          a.index,
		SigAlgo:        a.sigAlgo,
		HashAlgo:       a.hashAlgo,
		PrivateKey:     a.privateKey,
		Mnemonic:       a.mnemonic,
		DerivationPath: a.derivationPath,
	}
}

func (a *Bip44AccountKey) Validate() error {

	if !bip39.IsMnemonicValid(a.mnemonic) {
		return fmt.Errorf("invalid mnemonic defined for account in flow.json")
	}

	derivationPath, err := goeth.ParseDerivationPath(a.derivationPath)
	if err != nil {
		return fmt.Errorf("invalid derivation path defined for account in flow.json")
	}

	seed := bip39.NewSeed(a.mnemonic, "")
	curve := slip10.CurveBitcoin
	if a.sigAlgo == crypto.ECDSA_P256 {
		curve = slip10.CurveP256
	}
	accountKey, err := slip10.NewMasterKeyWithCurve(seed, curve)
	if err != nil {
		return err
	}

	for _, n := range derivationPath {
		accountKey, err = accountKey.NewChildKey(n)

		if err != nil {
			return err
		}
	}
	a.privateKey, err = crypto.DecodePrivateKey(a.SigAlgo(), accountKey.Key)
	if err != nil {
		return err
	}
	return nil
}

func (a *Bip44AccountKey) PrivateKeyHex() string {
	return hex.EncodeToString(a.privateKey.Encode())
}

func randomSeed(n int) ([]byte, error) {
	seed := make([]byte, n)

	_, err := rand.Read(seed)
	if err != nil {
		return nil, fmt.Errorf("failed to generate random seed: %v", err)
	}

	return seed, nil
}
