package main

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/adhikariprivate/rcfin-server/internal/collection"
	"github.com/adhikariprivate/rcfin-server/internal/database"
	"github.com/adhikariprivate/rcfin-server/internal/database/sqlite"
	"github.com/adhikariprivate/rcfin-server/internal/imageresize"
	"github.com/adhikariprivate/rcfin-server/internal/jellyfin"
	"github.com/adhikariprivate/rcfin-server/internal/muxnormalizer"
	"github.com/adhikariprivate/rcfin-server/internal/rclonestore"
)

type configFile struct {
	Listen struct {
		Address string
		Port    string
		TlsCert string
		TlsKey  string
		IPACL   string
	}
	Cachedir string
	Database struct {
		Sqlite sqlite.ConfigFile `yaml:"sqlite"`
	} `yaml:"database"`
	Logfile        string
	RescanInterval string `mapstructure:"rescan_interval"`
	Rclone         struct {
		Config   string `mapstructure:"config"`
		CacheDir string `mapstructure:"cache_dir"`
	} `mapstructure:"rclone"`
	Collections []struct {
		ID     string
		Name   string
		Type   string
		Remote string
		VFS    map[string]any `mapstructure:"vfs"`
	}
	Jellyfin struct {
		ServerID           string
		ServerName         string
		AutoRegister       bool
		QuickConnect       bool
		ImageQualityPoster int
	}
}

func main() {
	const configFileNameKey = "config"

	// Set up viper for config file and command line flags
	viper.SetConfigType("yaml")
	viper.SetDefault("listen.port", "8096")
	viper.SetDefault("logfile", "/dev/stdout")

	pflag.String("config", "rcfin.yaml", "Path to configuration file.")
	viper.BindPFlag(configFileNameKey, pflag.Lookup("config"))
	pflag.Parse()

	// Read config file
	cf := viper.GetString(configFileNameKey)
	log.Printf("Using config file %s", cf)
	viper.SetConfigFile(cf)
	if err := viper.ReadInConfig(); err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}
	var config configFile
	if err := viper.Unmarshal(&config); err != nil {
		log.Fatalf("Unable to decode config into struct: %v", err)
	}

	// Set up logging
	logfile := viper.GetString("logfile")
	log.Printf("Setting logfile to %s", logfile)
	switch logfile {
	case "none":
		log.SetOutput(io.Discard)
	default:
		f, err := os.OpenFile(logfile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0666)
		if err != nil {
			log.Fatalf("error opening file: %v", err)
		}
		defer f.Close()
		log.SetOutput(f)
	}

	log.Printf("dbinit")
	var err error
	var repo database.Repository
	if config.Database.Sqlite.Filename != "" {
		repo, err = database.New("sqlite", sqlite.ConfigFile{
			Filename: config.Database.Sqlite.Filename,
		})
	}
	if err != nil {
		log.Fatalf("database.New: %s", err.Error())
	}
	repo.StartBackgroundJobs(context.Background())
	if err := rclonestore.Configure(config.Rclone.Config, config.Rclone.CacheDir); err != nil {
		log.Fatalf("rclone configuration: %v", err)
	}
	rescanInterval := 30 * time.Minute
	if config.RescanInterval != "" {
		rescanInterval, err = time.ParseDuration(config.RescanInterval)
		if err != nil || rescanInterval <= 0 {
			log.Fatalf("invalid rescaninterval %q: expected a positive duration", config.RescanInterval)
		}
	}

	// Initialize collection and add them to the collection manager
	collection := collection.New(&collection.Options{
		Repo:           repo,
		RescanInterval: rescanInterval,
	})
	for _, coll := range config.Collections {
		storage, err := rclonestore.New(context.Background(), coll.Remote, coll.VFS)
		if err != nil {
			log.Fatalf("collection %q: %v", coll.Name, err)
		}
		collection.AddCollection(
			coll.Name,
			coll.ID,
			coll.Type,
			coll.Remote,
			storage,
		)
	}
	defer collection.Close()

	resizer := imageresize.New(imageresize.Options{
		Cachedir: config.Cachedir,
	})
	// Periodically evict stale entries from the local resized-image cache.
	if config.Cachedir != "" {
		go resizer.CleanCache(7*24*time.Hour, time.Hour)
	}

	log.Printf("building mux")

	r := mux.NewRouter()

	j := jellyfin.New(&jellyfin.Options{
		Collections:        collection,
		Repo:               repo,
		Imageresizer:       resizer,
		ServerPort:         config.Listen.Port,
		ServerID:           config.Jellyfin.ServerID,
		ServerName:         config.Jellyfin.ServerName,
		AutoRegister:       config.Jellyfin.AutoRegister,
		QuickConnect:       config.Jellyfin.QuickConnect,
		ImageQualityPoster: config.Jellyfin.ImageQualityPoster,
	})
	j.RegisterHandlers(r)

	r.Path("/robots.txt").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("User-agent: *\nDisallow: /\n"))
	})

	collection.Init()
	go collection.Background(context.Background())

	addr := net.JoinHostPort(config.Listen.Address, config.Listen.Port)

	// Add muxnormalizer middleware to canonicalize request paths and query parameters
	canon, err := muxnormalizer.New(r)
	if err != nil {
		log.Fatal(err)
	}
	server := HttpLog(IPACLmiddleware(config.Listen.IPACL, canon.Middleware(r)))

	if config.Listen.TlsCert != "" && config.Listen.TlsKey != "" {
		kpr, err := NewKeypairReloader(config.Listen.TlsCert, config.Listen.TlsKey)
		if err != nil {
			log.Fatalf("error loading keypair: %v", err)
		}

		srv := &http.Server{
			Addr:    addr,
			Handler: server,
			TLSConfig: &tls.Config{
				// Streamyfin's websocket connection still uses TLS1.2..
				MinVersion:     tls.VersionTLS12,
				GetCertificate: kpr.GetCertificateFunc(),
			},
		}
		log.Printf("Serving HTTPS on %s", addr)
		if err := srv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTPS server stopped: %v", err)
		}
	} else {
		log.Printf("Serving HTTP on %s", addr)
		if err := http.ListenAndServe(addr, server); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server stopped: %v", err)
		}
	}
}

type keypairReloader struct {
	certMu   sync.RWMutex
	cert     *tls.Certificate
	certPath string
	keyPath  string
}

// NewKeypairReloader creates a new keypair reloader that will reload the TLS certificate
// and key from the specified paths every 15 seconds. If the certificate cannot be loaded,
// it will log an error and keep the old certificate in use.
func NewKeypairReloader(certPath, keyPath string) (*keypairReloader, error) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	result := &keypairReloader{
		certPath: certPath,
		keyPath:  keyPath,
		cert:     &cert,
	}

	go func() {
		for {
			// log.Printf("Attemping reloading TLS certificate and key from %q and %q", certPath, keyPath)
			time.Sleep(15 * time.Second)
			if err := result.maybeReload(); err != nil {
				log.Printf("Keeping old TLS certificate because the new one could not be loaded: %v", err)
			}
		}
	}()
	return result, nil
}

func (kpr *keypairReloader) GetCertificateFunc() func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		kpr.certMu.RLock()
		defer kpr.certMu.RUnlock()
		return kpr.cert, nil
	}
}

func (kpr *keypairReloader) maybeReload() error {
	newCert, err := tls.LoadX509KeyPair(kpr.certPath, kpr.keyPath)
	if err != nil {
		return err
	}
	kpr.certMu.Lock()
	defer kpr.certMu.Unlock()
	kpr.cert = &newCert
	return nil
}

// IPACLmiddleware is an HTTP middleware enforcing IP allow list, if set.
func IPACLmiddleware(acl string, next http.Handler) http.Handler {
	// Don't process ACL in case it's empty
	acl = strings.TrimSpace(acl)
	if acl == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Get client ip without port
		remote := r.RemoteAddr
		if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			remote = host
		}
		clientIP := net.ParseIP(remote)
		if clientIP == nil {
			http.Error(w, "403 forbidden", http.StatusForbidden)
			return
		}
		allow := false
		for e := range strings.SplitSeq(acl, ",") {
			e := strings.TrimSpace(e)
			if strings.Contains(e, "/") {
				if _, cidr, err := net.ParseCIDR(e); err == nil {
					if cidr.Contains(clientIP) {
						allow = true
						break
					}
				} else {
					log.Printf("listen.ipacl: invalid CIDR %q: %v", e, err)
				}
				continue

			}
			if ip := net.ParseIP(e); ip == nil {
				if ip.Equal(clientIP) {
					allow = true
					break
				}
			} else {
				log.Printf("listen.ipacl: invalid IP %q", e)
			}
		}
		if !allow {
			http.Error(w, "403 forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
