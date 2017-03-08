package ssh

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	multierror "github.com/hashicorp/go-multierror"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
	"golang.org/x/crypto/ssh"
)

const (
	caPublicKey                       = "ca_public_key"
	caPrivateKey                      = "ca_private_key"
	caPublicKeyStoragePath            = "config/ca_public_key"
	caPublicKeyStoragePathDeprecated  = "public_key"
	caPrivateKeyStoragePath           = "config/ca_private_key"
	caPrivateKeyStoragePathDeprecated = "config/ca_bundle"
)

func pathConfigCA(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "config/ca",
		Fields: map[string]*framework.FieldSchema{
			"private_key": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: `Private half of the SSH key that will be used to sign certificates.`,
			},
			"public_key": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: `Public half of the SSH key that will be used to sign certificates.`,
			},
			"generate_signing_key": &framework.FieldSchema{
				Type:        framework.TypeBool,
				Description: `Generate SSH key pair internally rather than use the private_key and public_key fields.`,
				Default:     true,
			},
		},

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.UpdateOperation: b.pathConfigCAUpdate,
			logical.DeleteOperation: b.pathConfigCADelete,
		},

		HelpSynopsis: `Set the SSH private key used for signing certificates.`,
		HelpDescription: `This sets the CA information used for certificates generated by this
by this mount. The fields must be in the standard private and public SSH format.

For security reasons, the private key cannot be retrieved later.`,
	}
}

func (b *backend) pathConfigCADelete(
	req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	if err := req.Storage.Delete(caPrivateKeyStoragePath); err != nil {
		return nil, err
	}
	if err := req.Storage.Delete(caPublicKeyStoragePath); err != nil {
		return nil, err
	}
	return nil, nil
}

func caKey(storage logical.Storage, keyType string) (string, error) {
	var path, deprecatedPath string
	switch keyType {
	case caPrivateKey:
		path = caPrivateKeyStoragePath
		deprecatedPath = caPrivateKeyStoragePathDeprecated
	case caPublicKey:
		path = caPublicKeyStoragePath
		deprecatedPath = caPublicKeyStoragePathDeprecated
	default:
		return "", fmt.Errorf("unrecognized key type %q", keyType)
	}

	entry, err := storage.Get(path)
	if err != nil {
		return "", err
	}

	if entry == nil {
		// If the entry is not found, look at an older path. If found, upgrade
		// it.
		entry, err = storage.Get(deprecatedPath)
		if err != nil {
			return "", err
		}
		if entry != nil {
			err = storage.Put(&logical.StorageEntry{
				Key:   path,
				Value: entry.Value,
			})
			if err != nil {
				return "", err
			}
			if err = storage.Delete(deprecatedPath); err != nil {
				return "", err
			}
		}
	}
	if entry == nil {
		return "", nil
	}

	return string(entry.Value), nil
}

func (b *backend) pathConfigCAUpdate(req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	var err error
	publicKey := data.Get("public_key").(string)
	privateKey := data.Get("private_key").(string)

	var generateSigningKey bool

	generateSigningKeyRaw, ok := data.GetOk("generate_signing_key")
	switch {
	// explicitly set true
	case ok && generateSigningKeyRaw.(bool):
		if publicKey != "" || privateKey != "" {
			return logical.ErrorResponse("public_key and private_key must not be set when generate_signing_key is set to true"), nil
		}

		generateSigningKey = true

	// explicitly set to false, or not set and we have both a public and private key
	case ok, publicKey != "" && privateKey != "":
		if publicKey == "" {
			return logical.ErrorResponse("missing public_key"), nil
		}

		if privateKey == "" {
			return logical.ErrorResponse("missing private_key"), nil
		}

		_, err := ssh.ParsePrivateKey([]byte(privateKey))
		if err != nil {
			return logical.ErrorResponse(fmt.Sprintf("Unable to parse private_key as an SSH private key: %v", err)), nil
		}

		_, err = parsePublicSSHKey(publicKey)
		if err != nil {
			return logical.ErrorResponse(fmt.Sprintf("Unable to parse public_key as an SSH public key: %v", err)), nil
		}

	// not set and no public/private key provided so generate
	case publicKey == "" && privateKey == "":
		generateSigningKey = true

	// not set, but one or the other supplied
	default:
		return logical.ErrorResponse("only one of public_key and private_key set; both must be set to use, or both must be blank to auto-generate"), nil
	}

	if generateSigningKey {
		publicKey, privateKey, err = generateSSHKeyPair()
		if err != nil {
			return nil, err
		}
	}

	if publicKey == "" || privateKey == "" {
		return nil, fmt.Errorf("failed to generate or parse the keys")
	}

	storedPublicKey, err := caKey(req.Storage, caPublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA public key: %v", err)
	}

	storedPrivateKey, err := caKey(req.Storage, caPrivateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to read CA private key: %v", err)
	}

	if storedPublicKey != "" || storedPrivateKey != "" {
		return nil, fmt.Errorf("keys are already configured; delete them before reconfiguring")
	}

	err = req.Storage.Put(&logical.StorageEntry{
		Key:   caPublicKeyStoragePath,
		Value: []byte(publicKey),
	})
	if err != nil {
		return nil, err
	}

	err = req.Storage.Put(&logical.StorageEntry{
		Key:   caPrivateKeyStoragePath,
		Value: []byte(privateKey),
	})
	if err != nil {
		var mErr *multierror.Error

		mErr = multierror.Append(mErr, fmt.Errorf("failed to store CA private key: %v", err))

		// If storing private key fails, the corresponding public key should be
		// removed
		if delErr := req.Storage.Delete(caPublicKeyStoragePath); delErr != nil {
			mErr = multierror.Append(mErr, fmt.Errorf("failed to cleanup CA public key: %v", delErr))
			return nil, mErr
		}

		return nil, err
	}

	return nil, nil
}

func generateSSHKeyPair() (string, string, error) {
	privateSeed, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		return "", "", err
	}

	privateBlock := &pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: nil,
		Bytes:   x509.MarshalPKCS1PrivateKey(privateSeed),
	}

	public, err := ssh.NewPublicKey(&privateSeed.PublicKey)
	if err != nil {
		return "", "", err
	}

	return string(ssh.MarshalAuthorizedKey(public)), string(pem.EncodeToMemory(privateBlock)), nil
}
