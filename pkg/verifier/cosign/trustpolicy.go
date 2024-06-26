/*
Copyright The Ratify Authors.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cosign

import (
	"context"
	"crypto"
	"fmt"
	"os"
	"slices"

	re "github.com/deislabs/ratify/errors"
	"github.com/deislabs/ratify/internal/constants"
	"github.com/deislabs/ratify/pkg/keymanagementprovider"
	vu "github.com/deislabs/ratify/pkg/verifier/utils"
	"github.com/deislabs/ratify/utils"
	"github.com/sigstore/cosign/v2/cmd/cosign/cli/fulcio"
	"github.com/sigstore/cosign/v2/cmd/cosign/cli/rekor"
	"github.com/sigstore/cosign/v2/pkg/cosign"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
)

type KeyConfig struct {
	Provider string `json:"provider,omitempty"`
	Name     string `json:"name,omitempty"`
	Version  string `json:"version,omitempty"`
	File     string `json:"file,omitempty"`
}

type KeylessConfig struct {
	RekorURL    string `json:"rekorURL,omitempty"`
	CTLogVerify *bool  `json:"ctLogVerify,omitempty"`
}

type TrustPolicyConfig struct {
	Version    string        `json:"version"`
	Name       string        `json:"name"`
	Scopes     []string      `json:"scopes"`
	Keys       []KeyConfig   `json:"keys,omitempty"`
	Keyless    KeylessConfig `json:"keyless,omitempty"`
	TLogVerify *bool         `json:"tLogVerify,omitempty"`
}

type PKKey struct {
	Provider string `json:"provider"`
	Name     string `json:"name,omitempty"`
	Version  string `json:"version,omitempty"`
}

type trustPolicy struct {
	scopes       []string
	localKeys    map[PKKey]keymanagementprovider.PublicKey
	config       TrustPolicyConfig
	verifierName string
	isKeyless    bool
}

type TrustPolicy interface {
	GetName() string
	GetKeys(ctx context.Context, namespace string) (map[PKKey]keymanagementprovider.PublicKey, error)
	GetScopes() []string
	GetCosignOpts(context.Context) (cosign.CheckOpts, error)
}

const (
	fileProviderName                string = "file"
	DefaultRekorURL                 string = "https://rekor.sigstore.dev"
	DefaultTLogVerify               bool   = true
	DefaultCTLogVerify              bool   = true
	DefaultTrustPolicyConfigVersion string = "1.0.0"
)

var SupportedTrustPolicyConfigVersions = []string{DefaultTrustPolicyConfigVersion}

// CreateTrustPolicy creates a trust policy from the given configuration
// returns an error if the configuration is invalid
// reads the public keys from the file path
func CreateTrustPolicy(config TrustPolicyConfig, verifierName string) (TrustPolicy, error) {
	// set the default trust policy version if not provided
	// currently only one version is supported
	if config.Version == "" {
		config.Version = DefaultTrustPolicyConfigVersion
	}

	if err := validate(config, verifierName); err != nil {
		return nil, err
	}

	keyMap := make(map[PKKey]keymanagementprovider.PublicKey)
	for _, keyConfig := range config.Keys {
		// check if the key is defined by file path or by key management provider
		if keyConfig.File != "" {
			pubKey, err := loadKeyFromPath(keyConfig.File)
			if err != nil {
				return nil, re.ErrorCodeConfigInvalid.WithComponentType(re.Verifier).WithPluginName(verifierName).WithDetail(fmt.Sprintf("trust policy %s failed: failed to load key from file %s", config.Name, keyConfig.File)).WithError(err)
			}
			keyMap[PKKey{Provider: fileProviderName, Name: keyConfig.File}] = keymanagementprovider.PublicKey{Key: pubKey, ProviderType: fileProviderName}
		}
	}

	if config.Keyless.RekorURL == "" {
		config.Keyless.RekorURL = DefaultRekorURL
	}

	if config.TLogVerify == nil {
		config.TLogVerify = utils.MakePtr(DefaultTLogVerify)
	}

	if config.Keyless != (KeylessConfig{}) && config.Keyless.CTLogVerify == nil {
		config.Keyless.CTLogVerify = utils.MakePtr(DefaultCTLogVerify)
	}

	return &trustPolicy{
		scopes:       config.Scopes,
		localKeys:    keyMap,
		config:       config,
		verifierName: verifierName,
		isKeyless:    config.Keyless != KeylessConfig{},
	}, nil
}

// GetName returns the name of the trust policy
func (tp *trustPolicy) GetName() string {
	return tp.config.Name
}

// GetKeys returns the public keys defined in the trust policy
func (tp *trustPolicy) GetKeys(ctx context.Context, namespace string) (map[PKKey]keymanagementprovider.PublicKey, error) {
	keyMap := make(map[PKKey]keymanagementprovider.PublicKey)
	// preload the local keys into the map of keys to be returned
	for key, pubKey := range tp.localKeys {
		keyMap[key] = pubKey
	}

	for _, keyConfig := range tp.config.Keys {
		// if the key is defined by file path, it has already been loaded into the key map
		if keyConfig.File != "" {
			continue
		}
		// must prepend namespace to key management provider name if not provided since namespace is prepended during key management provider intialization
		namespacedKMP := prependNamespaceToKMPName(keyConfig.Provider, namespace)
		// get the key management provider resource which contains a map of keys
		kmpResource, ok := keymanagementprovider.GetKeysFromMap(ctx, namespacedKMP)
		if !ok {
			return nil, re.ErrorCodeConfigInvalid.WithComponentType(re.Verifier).WithPluginName(tp.verifierName).WithDetail(fmt.Sprintf("trust policy %s failed: key management provider %s not found", tp.config.Name, namespacedKMP))
		}
		// get a specific key from the key management provider resource
		if keyConfig.Name != "" {
			pubKey, exists := kmpResource[keymanagementprovider.KMPMapKey{Name: keyConfig.Name, Version: keyConfig.Version}]
			if !exists {
				return nil, re.ErrorCodeConfigInvalid.WithComponentType(re.Verifier).WithPluginName(tp.verifierName).WithDetail(fmt.Sprintf("trust policy %s failed: key %s with version %s not found in key management provider %s", tp.config.Name, keyConfig.Name, keyConfig.Version, namespacedKMP))
			}
			keyMap[PKKey{Provider: namespacedKMP, Name: keyConfig.Name, Version: keyConfig.Version}] = pubKey
		} else {
			// get all public keys from the key management provider
			for key, pubKey := range kmpResource {
				keyMap[PKKey{Provider: namespacedKMP, Name: key.Name, Version: key.Version}] = pubKey
			}
		}
	}
	return keyMap, nil
}

// GetScopes returns the scopes defined in the trust policy
func (tp *trustPolicy) GetScopes() []string {
	return tp.scopes
}

func (tp *trustPolicy) GetCosignOpts(ctx context.Context) (cosign.CheckOpts, error) {
	cosignOpts := cosign.CheckOpts{}
	if tp.isKeyless {
		roots, err := fulcio.GetRoots()
		if err != nil || roots == nil {
			return cosignOpts, fmt.Errorf("failed to get fulcio roots: %w", err)
		}
		cosignOpts.RootCerts = roots
		cosignOpts.RekorClient, err = rekor.NewClient(tp.config.Keyless.RekorURL)
		if err != nil {
			return cosignOpts, fmt.Errorf("failed to create Rekor client from URL %s: %w", tp.config.Keyless.RekorURL, err)
		}
		if tp.config.Keyless.CTLogVerify != nil && *tp.config.Keyless.CTLogVerify {
			cosignOpts.CTLogPubKeys, err = cosign.GetCTLogPubs(ctx)
			if err != nil {
				return cosignOpts, fmt.Errorf("failed to fetch certificate transparency log public keys: %w", err)
			}
		} else {
			cosignOpts.IgnoreSCT = true
		}
		// Fetches the Rekor public keys from the Rekor server
		cosignOpts.RekorPubKeys, err = cosign.GetRekorPubs(ctx)
		if err != nil {
			return cosignOpts, fmt.Errorf("failed to fetch Rekor public keys: %w", err)
		}
		cosignOpts.IntermediateCerts, err = fulcio.GetIntermediates()
		if err != nil {
			return cosignOpts, fmt.Errorf("failed to get fulcio intermediate certificates: %w", err)
		}
	}
	if tp.config.TLogVerify != nil && *tp.config.TLogVerify {
		cosignOpts.IgnoreTlog = true
	}
	return cosignOpts, nil
}

// validate checks if the trust policy configuration is valid
// returns an error if the configuration is invalid
func validate(config TrustPolicyConfig, verifierName string) error {
	// check if the trust policy version is supported
	if !slices.Contains(SupportedTrustPolicyConfigVersions, config.Version) {
		return re.ErrorCodeConfigInvalid.WithComponentType(re.Verifier).WithPluginName(verifierName).WithDetail(fmt.Sprintf("trust policy %s failed: unsupported version %s", config.Name, config.Version))
	}

	if config.Name == "" {
		return re.ErrorCodeConfigInvalid.WithComponentType(re.Verifier).WithPluginName(verifierName).WithDetail("missing trust policy name")
	}

	if len(config.Scopes) == 0 {
		return re.ErrorCodeConfigInvalid.WithComponentType(re.Verifier).WithPluginName(verifierName).WithDetail(fmt.Sprintf("trust policy %s failed: no scopes defined", config.Name))
	}

	// keys or keyless must be defined
	if len(config.Keys) == 0 && config.Keyless == (KeylessConfig{}) {
		return re.ErrorCodeConfigInvalid.WithComponentType(re.Verifier).WithPluginName(verifierName).WithDetail(fmt.Sprintf("trust policy %s failed: no keys defined and keyless section not configured", config.Name))
	}

	// only one of keys or keyless can be defined
	if len(config.Keys) > 0 && config.Keyless != (KeylessConfig{}) {
		return re.ErrorCodeConfigInvalid.WithComponentType(re.Verifier).WithPluginName(verifierName).WithDetail(fmt.Sprintf("trust policy %s failed: both keys and keyless sections are defined", config.Name))
	}

	for _, keyConfig := range config.Keys {
		// check if the key is defined by file path or by key management provider
		if keyConfig.File == "" && keyConfig.Provider == "" {
			return re.ErrorCodeConfigInvalid.WithComponentType(re.Verifier).WithPluginName(verifierName).WithDetail(fmt.Sprintf("trust policy %s failed: key management provider name is required when not using file path", config.Name))
		}
		// both file path and key management provider cannot be defined together
		if keyConfig.File != "" && keyConfig.Provider != "" {
			return re.ErrorCodeConfigInvalid.WithComponentType(re.Verifier).WithPluginName(verifierName).WithDetail(fmt.Sprintf("trust policy %s failed: 'name' and 'file' cannot be configured together", config.Name))
		}
		// key management provider is required when specific keys are configured
		if keyConfig.Name != "" && keyConfig.Provider == "" {
			return re.ErrorCodeConfigInvalid.WithComponentType(re.Verifier).WithPluginName(verifierName).WithDetail(fmt.Sprintf("trust policy %s failed: key management provider name is required when key name is defined", config.Name))
		}
		// key name is required when key version is defined
		if keyConfig.Version != "" && keyConfig.Name == "" {
			return re.ErrorCodeConfigInvalid.WithComponentType(re.Verifier).WithPluginName(verifierName).WithDetail(fmt.Sprintf("trust policy %s failed: key name is required when key version is defined", config.Name))
		}
	}

	return nil
}

// loadKeyFromPath loads a public key from a file path and returns it
// TODO: look into supporting cosign's blob.LoadFileOrURL to support URL + env variables
func loadKeyFromPath(filePath string) (crypto.PublicKey, error) {
	contents, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	if len(contents) == 0 {
		return nil, fmt.Errorf("key file %s is empty", filePath)
	}

	return cryptoutils.UnmarshalPEMToPublicKey(contents)
}

// prependNamespaceToKMPName prepends the namespace to the key management provider name if not already present
// if the namespace is empty, the key management provider name is returned as is
func prependNamespaceToKMPName(kmpName string, namespace string) string {
	// namespace will be empty for CLI scenarios. use the KMP name as is
	if vu.IsNamespacedNamed(kmpName) || namespace == "" {
		return kmpName
	}
	return fmt.Sprintf("%s%s%s", namespace, constants.NamespaceSeperator, kmpName)
}
