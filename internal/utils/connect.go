package utils

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgx/v4"
	"github.com/spf13/viper"
	"github.com/supabase/cli/internal/debug"
)

func isDialError(err error) bool {
	inner := errors.Unwrap(err)
	opErr, ok := inner.(*net.OpError)
	return ok && opErr.Op == "dial"
}

func ToPostgresURL(config pgconn.Config) string {
	timeoutSecond := int64(config.ConnectTimeout.Seconds())
	if timeoutSecond == 0 {
		timeoutSecond = 10
	}
	return fmt.Sprintf(
		"postgresql://%s@%s:%d/%s?connect_timeout=%d",
		url.UserPassword(config.User, config.Password),
		config.Host,
		config.Port,
		url.PathEscape(config.Database),
		timeoutSecond,
	)
}

// Connnect to remote Postgres with optimised settings. The caller is responsible for closing the connection returned.
func ConnectRemotePostgres(ctx context.Context, config pgconn.Config, options ...func(*pgx.ConnConfig)) (*pgx.Conn, error) {
	// Simple protocol is preferred over pgx default Parse -> Bind flow because
	//   1. Using a single command for each query reduces RTT over an Internet connection.
	//   2. Performance gains from using the alternate binary protocol is negligible because
	//      we are only selecting from migrations table. Large reads are handled by PostgREST.
	//   3. Any prepared statements are cleared server side upon closing the TCP connection.
	//      Since CLI workloads are one-off scripts, we don't use connection pooling and hence
	//      don't benefit from per connection server side cache.
	opts := append(options, func(cc *pgx.ConnConfig) {
		cc.PreferSimpleProtocol = true
		if DNSResolver.Value == DNS_OVER_HTTPS {
			cc.LookupFunc = FallbackLookupIP
		}
	})
	// Use port 6543 for connection pooling
	conn, err := ConnectByUrl(ctx, ToPostgresURL(config), opts...)
	if !pgconn.Timeout(err) && !isDialError(err) {
		return conn, err
	}
	if !ProjectHostPattern.MatchString(config.Host) || config.Port != 6543 {
		return conn, err
	}
	// Fallback to 5432 when pgbouncer is unavailable
	config.Port = 5432
	fmt.Fprintln(os.Stderr, "Retrying...", config.Host, config.Port)
	return ConnectByUrl(ctx, ToPostgresURL(config), opts...)
}

// Connnect to local Postgres with optimised settings. The caller is responsible for closing the connection returned.
func ConnectLocalPostgres(ctx context.Context, config pgconn.Config, options ...func(*pgx.ConnConfig)) (*pgx.Conn, error) {
	if len(config.Host) == 0 {
		config.Host = "localhost"
	}
	if config.Port == 0 {
		config.Port = uint16(Config.Db.Port)
	}
	if len(config.User) == 0 {
		config.User = "postgres"
	}
	if len(config.Password) == 0 {
		config.Password = Config.Db.Password
	}
	if len(config.Database) == 0 {
		config.Database = "postgres"
	}
	if config.ConnectTimeout == 0 {
		config.ConnectTimeout = 2 * time.Second
	}
	return ConnectByUrl(ctx, ToPostgresURL(config), options...)
}

func ConnectByUrl(ctx context.Context, url string, options ...func(*pgx.ConnConfig)) (*pgx.Conn, error) {
	// Parse connection url
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	// Apply config overrides
	for _, op := range options {
		op(config)
	}
	if viper.GetBool("DEBUG") {
		debug.SetupPGX(config)
	}
	// Connect to database
	return pgx.ConnectConfig(ctx, config)
}

func ConnectByConfig(ctx context.Context, config pgconn.Config, options ...func(*pgx.ConnConfig)) (*pgx.Conn, error) {
	if strings.ToLower(config.Host) == "localhost" {
		fmt.Fprintln(os.Stderr, "Connecting to local database...")
		return ConnectLocalPostgres(ctx, config, options...)
	}
	fmt.Fprintln(os.Stderr, "Connecting to remote database...")
	return ConnectRemotePostgres(ctx, config, options...)
}
