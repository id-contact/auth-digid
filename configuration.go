package main

import (
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"fmt"
	"io/ioutil"
	"net/url"

	jwtkeys "github.com/dgrijalva/jwt-go/v4"
	"github.com/spf13/viper"
)

type Configuration struct {
	// SAML service configuration
	SamlKeyPair    tls.Certificate
	IdpMetadataURL *url.URL
	EntityID       string // Not mandatory

	// Keys used to create attribute JWTs
	JwtSigningKey    *rsa.PrivateKey
	JwtEncryptionKey *rsa.PublicKey

	// BRP configuration
	BRPServer string
	Client    tls.Certificate
	CaCerts   []byte

	TestBSNMapping map[string]string

	// General server configuration
	ServerURL          *url.URL
	SamlSessionManager *SamlSessionEncoder
	SessionManager     *IDContactSessionManager
	DatabaseConnection string
	SentryDSN          string
	AttributeMapping   map[string]string
}

func ParseConfiguration() Configuration {
	// Setup configuration sources
	viper.SetConfigFile("config.json")
	viper.SetConfigType("json")
	viper.AddConfigPath(".")
	viper.SetEnvPrefix("DIGID")
	viper.AutomaticEnv()
	err := viper.ReadInConfig() // Find and read the config file
	if err != nil {             // Handle errors reading the config file
		panic(fmt.Errorf("Fatal error config file: %s \n", err))
	}

	// Load saml configuration
	samlCertificate := viper.GetString("SamlCertificate")
	samlKey := viper.GetString("SamlKey")
	keypair, err := tls.LoadX509KeyPair(samlCertificate, samlKey)
	if err != nil {
		fmt.Println("Failed to read saml keypair")
		panic(err)
	}
	keypair.Leaf, err = x509.ParseCertificate(keypair.Certificate[0])
	if err != nil {
		fmt.Println("Failed to parse leaf certificate")
		panic(err)
	}

	rawIdpURL := viper.GetString("IDPMetadataURL")
	idpMetadataURL, err := url.Parse(rawIdpURL)
	if err != nil {
		fmt.Println("Invalid identity provider metadata url")
		panic(err)
	}

	entityID := viper.GetString("EntityID")

	// Load BRP configuration
	caCertFile := viper.GetString("CACerts")
	caCerts, err := ioutil.ReadFile(caCertFile)
	if err != nil {
		fmt.Println("Failed to read ca certs")
		panic(err)
	}
	clientCertKey := viper.GetString("BRPKey")
	clientCertFile := viper.GetString("BRPCert")
	clientCert, err := tls.LoadX509KeyPair(clientCertFile, clientCertKey)
	if err != nil {
		fmt.Println("Failed to load brp key")
		panic(err)
	}

	// Load encryption keys
	jwtSigningKeyFile := viper.GetString("JWTSigningKey")
	jwtSigningKeyPEM, err := ioutil.ReadFile(jwtSigningKeyFile)
	if err != nil {
		fmt.Println("Failed to read jwt siging key")
		panic(err)
	}
	jwtSigningKey, err := jwtkeys.ParseRSAPrivateKeyFromPEM(jwtSigningKeyPEM)
	if err != nil {
		fmt.Println("Failed to parse jwt signing key")
		panic(err)
	}

	jwtEncryptionKeyFile := viper.GetString("JWTEncryptionKey")
	jwtEncryptionKeyPEM, err := ioutil.ReadFile(jwtEncryptionKeyFile)
	if err != nil {
		fmt.Println("Failed to read jwt encryption key")
		panic(err)
	}
	jwtEncryptionKey, err := jwtkeys.ParseRSAPublicKeyFromPEM(jwtEncryptionKeyPEM)

	// General server data
	rawServerURL := viper.GetString("ServerURL")
	serverURL, err := url.Parse(rawServerURL)
	databaseConnection := viper.GetString("DatabaseConnection")
	db, err := sql.Open("pgx", databaseConnection)
	if err != nil {
		fmt.Println("Couldn't open database")
		panic(err)
	}

	return Configuration{
		SamlKeyPair:    keypair,
		IdpMetadataURL: idpMetadataURL,
		EntityID:       entityID,

		JwtSigningKey:    jwtSigningKey,
		JwtEncryptionKey: jwtEncryptionKey,

		CaCerts:   caCerts,
		BRPServer: viper.GetString("BRPServer"),
		Client:    clientCert,

		ServerURL:          serverURL,
		DatabaseConnection: databaseConnection,
		SamlSessionManager: &SamlSessionEncoder{
			db: db,
		},
		SessionManager: &IDContactSessionManager{
			db: db,
		},
		AttributeMapping: viper.GetStringMapString("AttributeMapping"),
		TestBSNMapping:   viper.GetStringMapString("BSNMap"),
	}
}