package mysql

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/rds/auth"
	driverv2 "github.com/go-sql-driver/mysql"
	"github.com/na4ma4/mysql-role-reconciler/internal/config"
)

var ErrNoValidTLSConfig = errors.New("no valid TLS configuration provided")

// Connect establishes a database connection to the given server.
func Connect(ctx context.Context, srvCfg config.ServerConfig) (*sql.DB, error) {
	if srvCfg.Port == 0 {
		srvCfg.Port = 3306
	}

	var dsn string
	{
		var err error
		dsn, err = buildDSN(ctx, srvCfg)
		if err != nil {
			return nil, fmt.Errorf("building DSN: %w", err)
		}
	}

	var db *sql.DB
	{
		var err error
		db, err = sql.Open("mysql", dsn)
		if err != nil {
			return nil, fmt.Errorf("opening connection: %w", err)
		}
	}

	if srvCfg.OpenConnections.IsSet() {
		db.SetMaxOpenConns(srvCfg.OpenConnections.Get())
	}
	if srvCfg.IdleConnections.IsSet() {
		db.SetMaxIdleConns(srvCfg.IdleConnections.Get())
	}
	if srvCfg.MaxConnLifetime.IsSet() {
		db.SetConnMaxLifetime(srvCfg.MaxConnLifetime.Get())
	}

	if err := db.PingContext(ctx); err != nil {
		err = errors.Join(err, db.Close())
		return nil, fmt.Errorf("pinging server: %w", err)
	}

	return db, nil
}

func buildDSN(ctx context.Context, srvCfg config.ServerConfig) (string, error) {
	password := srvCfg.Password

	if srvCfg.IAMAuth {
		token, err := generateIAMToken(ctx, srvCfg)
		if err != nil {
			return "", fmt.Errorf("generating IAM auth token: %w", err)
		}
		password = token
	}

	host := srvCfg.Host
	if host == "" {
		return "", errors.New("host is required")
	}

	port := srvCfg.Port
	if port == 0 {
		port = 3306
	}

	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/", srvCfg.User, password, host, port)

	params := []string{"allowCleartextPasswords=1"}

	tlsConfig, err := buildTLSConfig(srvCfg)
	if err != nil && !errors.Is(err, ErrNoValidTLSConfig) {
		return "", fmt.Errorf("building TLS config: %w", err)
	}

	if tlsConfig != nil {
		tlsName := fmt.Sprintf("mysql-reconciler-%s", host)
		err = registerTLSConfigForDriver(tlsName, tlsConfig)
		if err != nil {
			return "", fmt.Errorf("registering TLS config: %w", err)
		}
		params = append(params, "tls="+tlsName)
	} else {
		params = append(params, "tls=true")
	}

	dsn += "?" + strings.Join(params, "&")

	return dsn, nil
}

func generateIAMToken(ctx context.Context, srvCfg config.ServerConfig) (string, error) {
	region := srvCfg.AWSRegion
	if region == "" {
		return "", errors.New("aws_region is required for IAM auth")
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return "", fmt.Errorf("loading AWS config: %w", err)
	}

	endpoint := fmt.Sprintf("%s:%d", srvCfg.Host, srvCfg.Port)
	if srvCfg.Port == 0 {
		endpoint = fmt.Sprintf("%s:3306", srvCfg.Host)
	}

	token, err := auth.BuildAuthToken(ctx, endpoint, region, srvCfg.User, cfg.Credentials)
	if err != nil {
		return "", fmt.Errorf("building auth token: %w", err)
	}

	return token, nil
}

func buildTLSConfig(srvCfg config.ServerConfig) (*tls.Config, error) {
	if srvCfg.SSL.CA == "" && srvCfg.SSL.Cert == "" && srvCfg.SSL.Key == "" {
		return nil, ErrNoValidTLSConfig
	}

	tlsCfg := &tls.Config{}

	if srvCfg.SSL.CA != "" {
		caData, err := os.ReadFile(srvCfg.SSL.CA)
		if err != nil {
			return nil, fmt.Errorf("reading CA file: %w", err)
		}
		caPool := x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caData) {
			return nil, errors.New("failed to append CA certificate")
		}
		tlsCfg.RootCAs = caPool
	}

	if srvCfg.SSL.Cert != "" && srvCfg.SSL.Key != "" {
		cert, err := tls.LoadX509KeyPair(srvCfg.SSL.Cert, srvCfg.SSL.Key)
		if err != nil {
			return nil, fmt.Errorf("loading client certificate: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}

	return tlsCfg, nil
}

func registerTLSConfigForDriver(name string, tlsCfg *tls.Config) error {
	return driverv2.RegisterTLSConfig(name, tlsCfg)
}
