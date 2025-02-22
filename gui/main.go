package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"math"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/biz/templates"
	"github.com/dustin/go-humanize"
	_ "github.com/go-sql-driver/mysql"
	"github.com/google/go-github/github"
	"github.com/gorilla/mux"
	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
	"github.com/gorilla/websocket"
	"github.com/nbutton23/zxcvbn-go"
	"github.com/spf13/viper"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	updateInterval = 24 * time.Hour
)

var (
	restartSecret   string
	sessionStore    *sessions.CookieStore
	tmpls           *templates.Templates
	version         string
	webTitle        string
	dbConn          string
	dbType          string
	isDev           bool
	updateAvailable bool
	updateChecked   time.Time
	srv             *http.Server
	configPath      string
	listenAddress   string

	//go:embed templates
	embeddedTemplates embed.FS
	//go:embed static
	staticFiles embed.FS
	// Is set by the compiler using -ldflags
	standaloneVersion string

	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
)

// User struct for storing the admin user account details.
type User struct {
	Name        string
	Email       string
	Password    string
	Confirm     string
	NewPassword string
	RequestBase string
	Errors      map[string]string
}

// ValidatePassword checks that the password of a User is non-empty, matches the confirmation, is not on the blacklist and is sufficiently complex.
func (reg *User) ValidatePassword(isNew bool, isChange bool) {
	blacklist := []string{"labca", "acme", reg.Name}
	if x := strings.Index(reg.Email, "@"); x > 0 {
		blacklist = append(blacklist, reg.Email[:x])
		d := strings.Split(reg.Email[x+1:], ".")
		for i := 0; i < len(d)-1; i++ {
			blacklist = append(blacklist, d[i])
		}
	}

	if strings.TrimSpace(reg.Password) == "" {
		reg.Errors["Password"] = "Please enter a password"
	} else if isNew {
		strength := zxcvbn.PasswordStrength(reg.Password, blacklist).Score
		if strength < 1 {
			reg.Errors["Password"] = "Please pick a stronger, more secure password"
		}
	}

	if isNew {
		if strings.TrimSpace(reg.Confirm) == "" {
			reg.Errors["Confirm"] = "Please enter the password again"
		} else if strings.TrimSpace(reg.Confirm) != strings.TrimSpace(reg.Password) {
			reg.Errors["Confirm"] = "Passwords do not match!"
		}
	}

	if isChange {
		if strings.TrimSpace(reg.NewPassword) != "" {
			strength := zxcvbn.PasswordStrength(reg.NewPassword, blacklist).Score
			if strength < 1 {
				reg.Errors["NewPassword"] = "Please pick a stronger, more secure password"
			}

			if strings.TrimSpace(reg.Confirm) == "" {
				reg.Errors["Confirm"] = "Please enter the new password again"
			} else if strings.TrimSpace(reg.Confirm) != strings.TrimSpace(reg.NewPassword) {
				reg.Errors["Confirm"] = "New passwords do not match!"
			}
		}

		byteStored := []byte(viper.GetString("user.password"))
		err := bcrypt.CompareHashAndPassword(byteStored, []byte(reg.Password))
		if err != nil {
			reg.Errors["Password"] = "Current password is not correct!"
		}
	}
}

// Validate that User struct contains at least a Name, has a valid email address and password fields.
func (reg *User) Validate(isNew bool, isChange bool) bool {
	reg.Errors = make(map[string]string)

	if strings.TrimSpace(reg.Name) == "" {
		reg.Errors["Name"] = "Please enter a user name"
	}

	if isNew || isChange {
		re := regexp.MustCompile(`.+@.+\..+`)
		matched := re.Match([]byte(reg.Email))
		if !matched {
			reg.Errors["Email"] = "Please enter a valid email address"
		}
	}

	reg.ValidatePassword(isNew, isChange)

	return len(reg.Errors) == 0
}

// SetupConfig stores the basic config settings.
type SetupConfig struct {
	Fqdn             string
	Organization     string
	WebTitle         string
	DNS              string
	DomainMode       string
	LockdownDomains  string
	WhitelistDomains string
	ExtendedTimeout  bool
	RequestBase      string
	Errors           map[string]string
}

// Validate that SetupConfig contains all required data.
func (cfg *SetupConfig) Validate(orgRequired bool) bool {
	cfg.Errors = make(map[string]string)

	if strings.TrimSpace(cfg.Fqdn) == "" {
		cfg.Errors["Fqdn"] = "Please enter the Fully Qualified Domain name for this host"
	}

	if strings.TrimSpace(cfg.Organization) == "" && orgRequired {
		cfg.Errors["Organization"] = "Please enter the organization name to show on the public pages"
	}

	if strings.TrimSpace(cfg.DNS) == "" {
		cfg.Errors["DNS"] = "Please enter the DNS server to be used for validation"
	}

	if cfg.DomainMode != "lockdown" && cfg.DomainMode != "whitelist" && cfg.DomainMode != "standard" {
		cfg.Errors["DomainMode"] = "Please select the domain mode to use"
	}

	if cfg.DomainMode == "lockdown" && strings.TrimSpace(cfg.LockdownDomains) == "" {
		cfg.Errors["LockdownDomains"] = "Please enter one or more domains that this PKI host is locked down to"
	}

	if cfg.DomainMode == "lockdown" && strings.HasPrefix(cfg.LockdownDomains, ".") {
		cfg.Errors["LockdownDomains"] = "Domain should not start with a dot"
	}

	if cfg.DomainMode == "whitelist" && strings.TrimSpace(cfg.WhitelistDomains) == "" {
		cfg.Errors["WhitelistDomains"] = "Please enter one or more domains that are whitelisted for this PKI host"
	}

	if cfg.DomainMode == "whitelist" && strings.HasPrefix(cfg.WhitelistDomains, ".") {
		cfg.Errors["WhitelistDomains"] = "Domain should not start with a dot"
	}

	return len(cfg.Errors) == 0
}

// StandaloneConfig stores the config settings when running standalone.
type StandaloneConfig struct {
	Backend     string
	MySQLServer string
	MySQLPort   string
	MySQLDBName string
	MySQLUser   string
	MySQLPasswd string
	UseHTTPS    bool
	CertPath    string
	KeyPath     string
	RequestBase string
	Errors      map[string]string
}

// Validate that StandaloneConfig contains all required data.
func (cfg *StandaloneConfig) Validate() bool {
	cfg.Errors = make(map[string]string)

	if strings.TrimSpace(cfg.Backend) != "step-ca" {
		cfg.Errors["Backend"] = "Currently only step-ca is supported as backend"
	}

	if strings.TrimSpace(cfg.MySQLServer) == "" {
		cfg.Errors["MySQLServer"] = "Please enter the name or IP address of the MySQL server"
	}
	_, err := strconv.Atoi(string(strings.TrimSpace(cfg.MySQLServer)[0]))
	if err == nil {
		if ip := net.ParseIP(strings.TrimSpace(cfg.MySQLServer)); ip == nil {
			cfg.Errors["MySQLServer"] = "Please enter a valid IP address"
		}
	}

	if strings.TrimSpace(cfg.MySQLPort) == "" {
		cfg.Errors["MySQLPort"] = "Please enter the port number of the MySQL server"
	}
	p, err := strconv.Atoi(strings.TrimSpace(cfg.MySQLPort))
	if err != nil || p < 1 || p > 65535 {
		cfg.Errors["MySQLPort"] = "Please enter a valid port number"
	}

	if strings.TrimSpace(cfg.MySQLDBName) == "" {
		cfg.Errors["MySQLDBName"] = "Please enter the name of the MySQL database"
	}

	if strings.TrimSpace(cfg.MySQLUser) == "" {
		cfg.Errors["MySQLUser"] = "Please enter the name of the MySQL user"
	}

	if strings.TrimSpace(cfg.MySQLPasswd) == "" {
		cfg.Errors["MySQLPasswd"] = "Please enter the password of the MySQL user"
	}

	if cfg.UseHTTPS && strings.TrimSpace(cfg.CertPath) == "" {
		cfg.Errors["CertPath"] = "Please enter the location and name of the HTTPS certificate to use"
	}

	if cfg.UseHTTPS && strings.TrimSpace(cfg.KeyPath) == "" {
		cfg.Errors["KeyPath"] = "Please enter the location and name of the HTTPS key file to use"
	}

	return len(cfg.Errors) == 0
}

func errorHandler(w http.ResponseWriter, r *http.Request, err error, status int) {
	log.Printf("errorHandler: err=%v\n", err)

	w.WriteHeader(status)

	pc := make([]uintptr, 15)
	n := runtime.Callers(2, pc)
	frames := runtime.CallersFrames(pc[:n])
	frame, _ := frames.Next()
	//fmt.Printf("%s:%d, %s\n", frame.File, frame.Line, frame.Function)

	if frame.Function == "main.render" {
		fmt.Fprintf(w, "Could not render requested page")
		return
	}

	if status == http.StatusNotFound {
		render(w, r, "error", map[string]interface{}{"Message": "That page does not exist"})
	} else {
		lines := strings.Split(string(debug.Stack()), "\n")
		if len(lines) >= 5 {
			lines = append(lines[:0], lines[5:]...)
		}
		fmt.Print(strings.Join(lines, "\n"))

		if viper.GetBool("config.complete") {
			render(w, r, "error", map[string]interface{}{"Message": "Some unexpected error occurred!"})
		} else {
			// ONLY in the setup phase to prevent leaking too much details to users
			var FileErrors []interface{}
			data := getLog(w, r, "cert")
			if data != "" {
				FileErrors = append(FileErrors, map[string]interface{}{"FileName": "/home/labca/nginx_data/ssl/acme_tiny.log", "Content": data})
			}
			data = getLog(w, r, "commander")
			if data != "" {
				FileErrors = append(FileErrors, map[string]interface{}{"FileName": "(control)/logs/commander.log", "Content": data})
			}
			data = getLog(w, r, "control-notail")
			if data != "" {
				FileErrors = append(FileErrors, map[string]interface{}{"FileName": "docker compose logs control", "Content": data})
			}
			data = getLog(w, r, "boulder-notail")
			if data != "" {
				FileErrors = append(FileErrors, map[string]interface{}{"FileName": "docker compose logs boulder", "Content": data})
			}
			data = getLog(w, r, "labca-notail")
			if data != "" {
				FileErrors = append(FileErrors, map[string]interface{}{"FileName": "docker compose logs labca", "Content": data})
			}

			render(w, r, "error", map[string]interface{}{"Message": "Some unexpected error occurred!", "FileErrors": FileErrors})
		}
	}
}

func checkUpdates(forced bool) ([]string, []string) {
	var versions []string
	var descriptions []string

	if forced || updateChecked.Add(updateInterval).Before(time.Now()) {
		latest := ""
		newer := true

		client := github.NewClient(nil)

		if releases, _, err := client.Repositories.ListReleases(context.Background(), "hakwerk", "labca", nil); err == nil {
			for i := 0; i < len(releases); i++ {
				release := releases[i]
				if !*release.Draft {
					if !*release.Prerelease || isDev {
						if latest == "" {
							latest = *release.Name
						}
						if *release.Name == version {
							newer = false
						}
						if latest == *release.Name && strings.HasPrefix(version, *release.Name+"-") { // git describe format
							newer = false
							latest = version
						}
						if newer {
							versions = append(versions, *release.Name)
							descriptions = append(descriptions, *release.Body)
						}
					}
				}
			}

			updateChecked = time.Now()
			updateAvailable = (len(releases) > 0) && (latest != version)
		}
	}

	return versions, descriptions
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	if !viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	dashboardData, err := CollectDashboardData(w, r)
	if err == nil {
		if viper.GetBool("standalone") {
			dashboardData["UpdateAvailable"] = false
		} else {
			checkUpdates(false)
			dashboardData["UpdateAvailable"] = updateAvailable
		}
		dashboardData["UpdateChecked"] = strings.Replace(updateChecked.Format("02-Jan-2006 15:04:05 MST"), "+0000", "GMT", -1)
		dashboardData["UpdateCheckedRel"] = humanize.RelTime(updateChecked, time.Now(), "", "")

		render(w, r, "dashboard", dashboardData)
	}
}

func aboutHandler(w http.ResponseWriter, r *http.Request) {
	render(w, r, "about", map[string]interface{}{
		"Title":      "About",
		"Standalone": viper.GetBool("standalone"),
	})
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	if viper.Get("user.password") == nil {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	session, _ := sessionStore.Get(r, "labca")
	var bounceURL string
	if session.Values["bounce"] == nil {
		bounceURL = "/"
	} else {
		bounceURL = session.Values["bounce"].(string)
	}

	if session.Values["user"] != nil {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+bounceURL, http.StatusFound)
		return
	}

	if r.Method == "GET" {
		reg := &User{
			RequestBase: r.Header.Get("X-Request-Base"),
		}
		render(w, r, "login", map[string]interface{}{"User": reg, "IsLogin": true})
		return
	} else if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			errorHandler(w, r, err, http.StatusInternalServerError)
			return
		}

		reg := &User{
			Name:        r.Form.Get("username"),
			Password:    r.Form.Get("password"),
			RequestBase: r.Header.Get("X-Request-Base"),
		}

		if !reg.Validate(false, false) {
			render(w, r, "login", map[string]interface{}{"User": reg, "IsLogin": true})
			return
		}

		if viper.GetString("user.name") != reg.Name {
			reg.Errors["Name"] = "Incorrect username or password"
			render(w, r, "login", map[string]interface{}{"User": reg, "IsLogin": true})
			return
		}

		byteStored := []byte(viper.GetString("user.password"))
		err := bcrypt.CompareHashAndPassword(byteStored, []byte(reg.Password))
		if err != nil {
			log.Println(err)
			reg.Errors["Name"] = "Incorrect username or password"
			render(w, r, "login", map[string]interface{}{"User": reg, "IsLogin": true})
			return
		}

		session.Values["user"] = reg.Name
		if err = session.Save(r, w); err != nil {
			log.Printf("cannot save session: %s\n", err)
		}

		http.Redirect(w, r, r.Header.Get("X-Request-Base")+bounceURL, http.StatusFound)
	} else {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/login", http.StatusSeeOther)
		return
	}
}

func logoutHandler(w http.ResponseWriter, r *http.Request) {
	session, _ := sessionStore.Get(r, "labca")
	session.Options.MaxAge = -1
	if err := session.Save(r, w); err != nil {
		log.Printf("cannot save session: %s\n", err)
	}
	http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/", http.StatusFound)
}

func _sendCmdOutput(w http.ResponseWriter, r *http.Request, cmd string) {
	parts := strings.Fields(cmd)
	for i := 0; i < len(parts); i++ {
		parts[i] = strings.Replace(parts[i], "\\\\", " ", -1)
	}
	head := parts[0]
	parts = parts[1:]

	out, err := exec.Command(head, parts...).Output()
	if err != nil {
		errorHandler(w, r, err, http.StatusInternalServerError)
		return
	}

	buf := bytes.NewBuffer(out)
	_, err = buf.WriteTo(w)
	if err != nil {
		errorHandler(w, r, err, http.StatusInternalServerError)
		return
	}
}

func _backupHandler(w http.ResponseWriter, r *http.Request) {
	res := struct {
		Success bool
		Message string
	}{Success: true}

	action := r.Form.Get("action")
	if action == "backup-restore" {
		backup := r.Form.Get("backup")
		if !_hostCommand(w, r, action, backup) {
			res.Success = false
			res.Message = "Command failed - see LabCA log for any details"
		} else {
			defer func() {
				_hostCommand(w, r, "server-restart")
				if _, err := exeCmd("./restart_control"); err != nil {
					log.Printf("_backupHandler: error restarting control container: %v", err)
				}
			}()
		}
	} else if action == "backup-delete" {
		backup := r.Form.Get("backup")
		if !_hostCommand(w, r, action, backup) {
			res.Success = false
			res.Message = "Command failed - see LabCA log for any details"
		}
	} else if action == "backup-now" {
		res.Message = getLog(w, r, "server-backup")
		if res.Message == "" {
			res.Success = false
			res.Message = "Command failed - see LabCA log for any details"
		} else {
			res.Message = filepath.Base(res.Message)
		}
	} else if action == "backup-upload" {
		file, header, err := r.FormFile("backup-file")
		if err != nil {
			fmt.Println(err)
			res.Success = false
			res.Message = "Could not read uploaded file"
		}
		var out *os.File
		if res.Success {
			defer file.Close()

			out, err = os.Create("/opt/backup/" + header.Filename)
			if err != nil {
				fmt.Println(err)
				res.Success = false
				res.Message = "Could not create backup file on server"
			}
		}
		if res.Success {
			defer out.Close()

			_, copyError := io.Copy(out, file)
			if copyError != nil {
				fmt.Println(err)
				res.Success = false
				res.Message = "Could not store uploaded file"
			} else {
				res.Message = header.Filename
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func _accountUpdateHandler(w http.ResponseWriter, r *http.Request) {
	reg := &User{
		Name:        r.Form.Get("username"),
		Email:       r.Form.Get("email"),
		NewPassword: r.Form.Get("new-password"),
		Confirm:     r.Form.Get("confirm"),
		Password:    r.Form.Get("password"),
	}

	res := struct {
		Success bool
		Errors  map[string]string
	}{Success: true}

	if reg.Validate(false, true) {
		viper.Set("user.name", reg.Name)
		viper.Set("user.email", reg.Email)

		if reg.NewPassword != "" {
			hash, err := bcrypt.GenerateFromPassword([]byte(reg.NewPassword), bcrypt.MinCost)
			if err != nil {
				res.Success = false
				errorHandler(w, r, err, http.StatusInternalServerError)
				return
			}
			viper.Set("user.password", string(hash))

			// Forget current session, so user has to login with the new password
			session, _ := sessionStore.Get(r, "labca")
			session.Options.MaxAge = -1
			if err = session.Save(r, w); err != nil {
				log.Printf("cannot save session: %s\n", err)
			}
		}

		viper.WriteConfig()

	} else {
		res.Success = false
		res.Errors = reg.Errors
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func backendUpdateHandler(w http.ResponseWriter, r *http.Request) {
	cfg := &StandaloneConfig{
		Backend:     r.Form.Get("backend"),
		MySQLServer: r.Form.Get("mysql_server"),
		MySQLPort:   r.Form.Get("mysql_port"),
		MySQLDBName: r.Form.Get("mysql_dbname"),
		MySQLUser:   r.Form.Get("mysql_user"),
		MySQLPasswd: r.Form.Get("mysql_passwd"),
		UseHTTPS:    (r.Form.Get("use_https") == "https"),
		CertPath:    r.Form.Get("cert_path"),
		KeyPath:     r.Form.Get("key_path"),
		RequestBase: r.Header.Get("X-Request-Base"),
	}

	res := struct {
		Success bool
		Errors  map[string]string
	}{Success: true}

	if cfg.Validate() {
		writeStandaloneConfig(cfg)
	} else {
		res.Success = false
		res.Errors = cfg.Errors
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func _configUpdateHandler(w http.ResponseWriter, r *http.Request) {
	cfg := &SetupConfig{
		Fqdn:             r.Form.Get("fqdn"),
		Organization:     r.Form.Get("organization"),
		WebTitle:         r.Form.Get("webtitle"),
		DNS:              r.Form.Get("dns"),
		DomainMode:       r.Form.Get("domain_mode"),
		LockdownDomains:  r.Form.Get("lockdown_domains"),
		WhitelistDomains: r.Form.Get("whitelist_domains"),
		ExtendedTimeout:  (r.Form.Get("extended_timeout") == "true"),
	}

	res := struct {
		Success bool
		Errors  map[string]string
	}{Success: true}

	if cfg.Validate(true) {
		delta := false
		deltaFQDN := false

		if cfg.Fqdn != viper.GetString("labca.fqdn") {
			delta = true
			deltaFQDN = true
			viper.Set("labca.fqdn", cfg.Fqdn)
		}

		if cfg.Organization != viper.GetString("labca.organization") {
			delta = true
			viper.Set("labca.organization", cfg.Organization)
		}

		if cfg.WebTitle != viper.GetString("labca.web_title") {
			delta = true
			viper.Set("labca.web_title", cfg.WebTitle)
		}

		matched, err := regexp.MatchString(":\\d+$", cfg.DNS)
		if err == nil && !matched {
			cfg.DNS += ":53"
		}

		if cfg.DNS != viper.GetString("labca.dns") {
			delta = true
			viper.Set("labca.dns", cfg.DNS)
		}

		domainMode := cfg.DomainMode
		if domainMode != viper.GetString("labca.domain_mode") {
			delta = true
			viper.Set("labca.domain_mode", cfg.DomainMode)
		}

		if domainMode == "lockdown" {
			if cfg.LockdownDomains != viper.GetString("labca.lockdown") {
				delta = true
				viper.Set("labca.lockdown", cfg.LockdownDomains)
			}
		}
		if domainMode == "whitelist" {
			if cfg.WhitelistDomains != viper.GetString("labca.whitelist") {
				delta = true
				viper.Set("labca.whitelist", cfg.WhitelistDomains)
			}
		}

		extendedTimeout := cfg.ExtendedTimeout
		if extendedTimeout != viper.GetBool("labca.extended_timeout") {
			delta = true
			viper.Set("labca.extended_timeout", cfg.ExtendedTimeout)
		}

		if delta {
			viper.WriteConfig()

			webTitle = viper.GetString("labca.web_title")
			if webTitle == "" {
				webTitle = "LabCA"
			}

			err := _applyConfig()
			if err != nil {
				res.Success = false
				res.Errors = cfg.Errors
				res.Errors["ConfigUpdate"] = "Config apply error: '" + err.Error() + "'"
			} else if deltaFQDN {
				if !_hostCommand(w, r, "acme-change", viper.GetString("labca.fqdn")) {
					res.Success = false
					res.Errors = cfg.Errors
					res.Errors["ConfigUpdate"] = "Error requesting certificate for new fqdn"
				}
			}
		} else {
			res.Success = false
			res.Errors = cfg.Errors
			res.Errors["ConfigUpdate"] = "Nothing changed!"
		}

	} else {
		res.Success = false
		res.Errors = cfg.Errors
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func _crlIntervalUpdateHandler(w http.ResponseWriter, r *http.Request) {
	res := struct {
		Success bool
		Errors  map[string]string
	}{Success: true, Errors: make(map[string]string)}

	delta := false
	crlInterval := r.Form.Get("crl_interval")

	ci, err := time.ParseDuration(crlInterval)
	if err != nil {
		res.Success = false
		res.Errors["CRLInterval"] = "Could not parse duration"
	} else {
		back := 4 * ci
		crlInterval += "|" + back.String()
		if crlInterval != viper.GetString("crl_interval") {
			delta = true
			viper.Set("crl_interval", crlInterval)
		}

		if delta {
			viper.WriteConfig()

			err := _applyConfig()
			if err != nil {
				res.Success = false
				res.Errors["CRLInterval"] = "Config apply error: '" + err.Error() + "'"
			} else if !_hostCommand(w, r, "boulder-restart") {
				res.Success = false
				res.Errors["CRLInterval"] = "Error restarting Boulder (ACME)"
			}
		} else {
			res.Success = false
			res.Errors["CRLInterval"] = "Nothing changed!"
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

// EmailConfig stores configuration used for sending out emails
type EmailConfig struct {
	DoEmail   bool
	Server    string
	Port      string
	EmailUser string
	EmailPwd  []byte
	From      string
	Errors    map[string]string
}

// Validate that the email config is valid and complete
func (cfg *EmailConfig) Validate() bool {
	cfg.Errors = make(map[string]string)

	result, err := _encrypt(cfg.EmailPwd)
	if err == nil {
		cfg.EmailPwd = []byte(result)
	} else {
		cfg.Errors["EmailPwd"] = "Could not encrypt this password: " + err.Error()
	}

	if !cfg.DoEmail {
		return len(cfg.Errors) == 0
	}

	if strings.TrimSpace(cfg.Server) == "" {
		cfg.Errors["Server"] = "Please enter the email server address"
	}

	if strings.TrimSpace(cfg.Port) == "" {
		cfg.Errors["Port"] = "Please enter the email server port number"
	}

	p, err := strconv.Atoi(cfg.Port)
	if err != nil {
		cfg.Errors["Port"] = "Port number must be numeric"
	} else if p <= 0 {
		cfg.Errors["Port"] = "Port number must be positive"
	} else if p > 65535 {
		cfg.Errors["Port"] = "Port number too large"
	}

	if strings.TrimSpace(cfg.EmailUser) == "" {
		cfg.Errors["EmailUser"] = "Please enter the username for authorization to the email server"
	}

	res, err := _decrypt(string(cfg.EmailPwd))
	if err != nil {
		cfg.Errors["EmailPwd"] = "Could not decrypt this password: " + err.Error()
	}
	if strings.TrimSpace(string(res)) == "" {
		cfg.Errors["EmailPwd"] = "Please enter the password for authorization to the email server"
	}

	if strings.TrimSpace(cfg.From) == "" {
		cfg.Errors["From"] = "Please enter the from email address"
	}

	return len(cfg.Errors) == 0
}

func _emailUpdateHandler(w http.ResponseWriter, r *http.Request) {
	cfg := &EmailConfig{
		DoEmail:   (r.Form.Get("do_email") == "true"),
		Server:    r.Form.Get("server"),
		Port:      r.Form.Get("port"),
		EmailUser: r.Form.Get("email_user"),
		EmailPwd:  []byte(r.Form.Get("email_pwd")),
		From:      r.Form.Get("from"),
	}

	res := struct {
		Success bool
		Errors  map[string]string
	}{Success: true}

	if cfg.Validate() {
		delta := false

		if cfg.DoEmail != viper.GetBool("labca.email.enable") {
			delta = true
			viper.Set("labca.email.enable", cfg.DoEmail)
		}

		if cfg.Server != viper.GetString("labca.email.server") {
			delta = true
			viper.Set("labca.email.server", cfg.Server)
		}

		if cfg.Port != viper.GetString("labca.email.port") {
			delta = true
			viper.Set("labca.email.port", cfg.Port)
		}

		if cfg.EmailUser != viper.GetString("labca.email.user") {
			delta = true
			viper.Set("labca.email.user", cfg.EmailUser)
		}

		res1, err1 := _decrypt(string(cfg.EmailPwd))
		if err1 != nil && cfg.DoEmail {
			log.Println("WARNING: could not decrypt given password: " + err1.Error())
		}
		res2, err2 := _decrypt(viper.GetString("labca.email.pass"))
		if err2 != nil && cfg.DoEmail && viper.GetString("labca.email.pass") != "" {
			log.Println("WARNING: could not decrypt stored password: " + err2.Error())
		}
		if string(res1) != string(res2) {
			delta = true
			viper.Set("labca.email.pass", string(cfg.EmailPwd))
		}

		if cfg.From != viper.GetString("labca.email.from") {
			delta = true
			viper.Set("labca.email.from", cfg.From)
		}

		if delta {
			viper.WriteConfig()

			err := _applyConfig()
			if err != nil {
				res.Success = false
				res.Errors = cfg.Errors
				res.Errors["EmailUpdate"] = "Config apply error: '" + err.Error() + "'"
			}
		} else {
			res.Success = false
			res.Errors = cfg.Errors
			res.Errors["EmailUpdate"] = "Nothing changed!"
		}

	} else {
		res.Success = false
		res.Errors = cfg.Errors
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func _emailSendHandler(w http.ResponseWriter, r *http.Request) {
	res := struct {
		Success bool
		Errors  map[string]string
	}{Success: true, Errors: make(map[string]string)}

	recipient := viper.GetString("user.email")
	if !_hostCommand(w, r, "test-email", recipient) {
		res.Success = false
		res.Errors["EmailSend"] = "Failed to send email - see logs"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func _exportHandler(w http.ResponseWriter, r *http.Request) {
	basename := "certificates"
	if r.Form.Get("root") != "true" {
		basename = "issuer"
	}
	if r.Form.Get("issuer") != "true" {
		basename = "root"
	}

	if r.Form.Get("type") == "pfx" {
		w.Header().Set("Content-Type", "application/x-pkcs12")
		w.Header().Set("Content-Disposition", "attachment; filename=labca_"+basename+".pfx")

		var certBase string
		if basename == "root" {
			certBase = "data/root-ca"
		} else {
			certBase = "data/issuer/ca-int"
		}

		cmd := "openssl pkcs12 -export -inkey " + certBase + ".key -in " + certBase + ".pem -passout pass:" + r.Form.Get("export-pwd")

		_sendCmdOutput(w, r, cmd)
	}

	if r.Form.Get("type") == "zip" {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename=labca_"+basename+".zip")

		cmd := "zip -j -P " + r.Form.Get("export-pwd") + " - "
		var certBase string
		if r.Form.Get("root") == "true" {
			certBase = "data/root-ca"
			cmd = cmd + certBase + ".key " + certBase + ".pem "
		}
		if r.Form.Get("issuer") == "true" {
			certBase = "data/issuer/ca-int"
			cmd = cmd + certBase + ".key " + certBase + ".pem "
		}

		_sendCmdOutput(w, r, cmd)
	}
}

func _doCmdOutput(w http.ResponseWriter, r *http.Request, cmd string) string {
	parts := strings.Fields(cmd)
	for i := 0; i < len(parts); i++ {
		parts[i] = strings.Replace(parts[i], "\\\\", " ", -1)
	}
	head := parts[0]
	parts = parts[1:]

	out, err := exec.Command(head, parts...).Output()
	if err != nil {
		errorHandler(w, r, err, http.StatusInternalServerError)
		return ""
	}

	return string(out)
}

func _encrypt(plaintext []byte) (string, error) {
	key := []byte(viper.GetString("keys.enc"))
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	_, err = io.ReadFull(rand.Reader, nonce)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plaintext, nil)), nil
}

func _decrypt(ciphertext string) ([]byte, error) {
	key := []byte(viper.GetString("keys.enc"))
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	ct, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return nil, err
	}

	if len(ct) < gcm.NonceSize() {
		return nil, errors.New("malformed ciphertext")
	}

	return gcm.Open(nil, ct[:gcm.NonceSize()], ct[gcm.NonceSize():], nil)
}

// Result contains data on managed processes
type Result struct {
	Success      bool
	Message      string
	Timestamp    string
	TimestampRel string
	Class        string
}

// ManageComponents sets the additional data to be displayed on the page for the LabCA components
func (res *Result) ManageComponents(w http.ResponseWriter, r *http.Request, action string) {
	components := _parseComponents(getLog(w, r, "components"))
	for i := 0; i < len(components); i++ {
		if (components[i].Name == "NGINX Webserver" && (action == "nginx-reload" || action == "nginx-restart")) ||
			(components[i].Name == "LabCA Controller" && action == "svc-restart") ||
			(components[i].Name == "Boulder (ACME)" && (action == "boulder-start" || action == "boulder-stop" || action == "boulder-restart")) ||
			(components[i].Name == "LabCA Application" && action == "labca-restart") ||
			(components[i].Name == "Consul (Boulder)" && action == "consul-restart") ||
			(components[i].Name == "MySQL Database" && action == "mysql-restart") {
			res.Timestamp = components[i].Timestamp
			res.TimestampRel = components[i].TimestampRel
			res.Class = components[i].Class
			break
		}
	}
}

func _checkUpdatesHandler(w http.ResponseWriter, r *http.Request) {
	res := struct {
		Success          bool
		UpdateAvailable  bool
		UpdateChecked    string
		UpdateCheckedRel string
		Versions         []string
		Descriptions     []string
		Errors           map[string]string
	}{Success: true, Errors: make(map[string]string)}

	res.Versions, res.Descriptions = checkUpdates(true)
	res.UpdateAvailable = updateAvailable
	res.UpdateChecked = updateChecked.Format("02-Jan-2006 15:04:05 MST")
	res.UpdateChecked = strings.Replace(res.UpdateChecked, "+0000", "GMT", -1)
	res.UpdateCheckedRel = humanize.RelTime(updateChecked, time.Now(), "", "")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func generateCRLHandler(w http.ResponseWriter, r *http.Request, isRoot bool) {
	res := struct {
		Success bool
		Errors  map[string]string
	}{Success: true, Errors: make(map[string]string)}

	if isRoot {
		path := "data/"
		certBase := "root-ca"
		keyFileExists := true
		if _, err := os.Stat(path + certBase + ".key"); os.IsNotExist(err) {
			keyFileExists = false
		}
		if keyFileExists {
			if _, err := exeCmd("openssl ca -config " + path + "openssl.cnf -gencrl -keyfile " + path + certBase + ".key -cert " + path + certBase + ".pem -out " + path + certBase + ".crl"); err != nil {
				res.Success = false
				res.Errors["CRL"] = "Could not generate Root CRL - see logs"
			}
		} else {
			if r.Form.Get("rootkey") == "" {
				res.Success = false
				res.Errors["CRL"] = "NO_ROOT_KEY"
			} else {
				rootci := &CertificateInfo{
					IsRoot:     true,
					Key:        r.Form.Get("rootkey"),
					Passphrase: r.Form.Get("rootpassphrase"),
				}
				if !rootci.StoreRootKey(path) {
					res.Success = false
					res.Errors["CRL"] = rootci.Errors["Modal"]
				} else {
					// Generate CRL now that we have the key
					if _, err := exeCmd("openssl ca -config " + path + "openssl.cnf -gencrl -keyfile " + path + certBase + ".key -cert " + path + certBase + ".pem -out " + path + certBase + ".crl"); err != nil {
						res.Success = false
						res.Errors["CRL"] = "Could not generate Root CRL - see logs"
					}
					// Remove the Root Key if we want to keep it offline
					if viper.GetBool("keep_root_offline") {
						if _, err := os.Stat(path + certBase + ".key"); !os.IsNotExist(err) {
							fmt.Println("Removing private Root key from the system...")
							if _, err := exeCmd("rm " + path + certBase + ".key"); err != nil {
								log.Printf("_certCreate: error deleting root key: %v", err)
							}
						}
						if _, err := os.Stat(path + certBase + ".key.der"); !os.IsNotExist(err) {
							if _, err := exeCmd("rm " + path + certBase + ".key.der"); err != nil {
								log.Printf("_certCreate: error deleting root key (DER format): %v", err)
							}
						}
					}
				}
			}
		}

		_hostCommand(w, r, "check-crl")

	} else { // !isRoot
		if !_hostCommand(w, r, "gen-issuer-crl") {
			res.Success = false
			res.Errors["CRL"] = "Failed to generate CRL - see logs"
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func uploadCRLHandler(w http.ResponseWriter, r *http.Request) {
	res := struct {
		Success bool
		Errors  map[string]string
	}{Success: true, Errors: make(map[string]string)}

	rootci := &CertificateInfo{
		IsRoot: true,
		CRL:    r.Form.Get("crl"),
	}
	if !rootci.StoreCRL("data/") {
		res.Success = false
		res.Errors["CRL"] = rootci.Errors["Modal"]
	}

	_hostCommand(w, r, "check-crl")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func _managePostDispatch(w http.ResponseWriter, r *http.Request, action string) bool {
	if action == "backup-restore" || action == "backup-delete" || action == "backup-now" || action == "backup-upload" {
		_backupHandler(w, r)
		return true
	}

	if action == "cert-export" {
		_exportHandler(w, r)
		return true
	}

	if action == "update-account" {
		_accountUpdateHandler(w, r)
		return true
	}

	if action == "update-backend" {
		backendUpdateHandler(w, r)
		return true
	}

	if action == "update-config" {
		_configUpdateHandler(w, r)
		return true
	}

	if action == "update-crl-interval" {
		_crlIntervalUpdateHandler(w, r)
		return true
	}

	if action == "update-email" {
		_emailUpdateHandler(w, r)
		return true
	}

	if action == "send-email" {
		_emailSendHandler(w, r)
		return true
	}

	if action == "version-check" {
		_checkUpdatesHandler(w, r)
		return true
	}

	if action == "upload-root-crl" {
		uploadCRLHandler(w, r)
		return true
	}

	if action == "gen-root-crl" {
		generateCRLHandler(w, r, true)
		return true
	}

	if action == "gen-issuer-crl" {
		generateCRLHandler(w, r, false)
		return true
	}

	if action == "svc-restart" {
		if _, err := exeCmd("./restart_control"); err != nil {
			log.Printf("_managePostDispatch: error restarting control container: %v", err)
		}
		return true
	}

	return false
}

func _managePost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		errorHandler(w, r, err, http.StatusInternalServerError)
		return
	}

	action := r.Form.Get("action")
	if action == "" {
		if err := r.ParseMultipartForm(2 * 1024 * 1024); err != nil {
			errorHandler(w, r, err, http.StatusInternalServerError)
			return
		}
		action = r.Form.Get("action")
	}

	actionKnown := false
	for _, a := range []string{
		"backup-restore",
		"backup-delete",
		"backup-now",
		"backup-upload",
		"cert-export",
		"mysql-restart",
		"consul-restart",
		"nginx-reload",
		"nginx-restart",
		"svc-restart",
		"boulder-start",
		"boulder-stop",
		"boulder-restart",
		"labca-restart",
		"server-restart",
		"update-account",
		"update-backend",
		"update-config",
		"update-crl-interval",
		"update-email",
		"send-email",
		"version-check",
		"version-update",
		"upload-root-crl",
		"gen-root-crl",
		"gen-issuer-crl",
	} {
		if a == action {
			actionKnown = true
		}
	}
	if !actionKnown {
		errorHandler(w, r, fmt.Errorf("unknown manage action '%s'", action), http.StatusBadRequest)
		return
	}

	if _managePostDispatch(w, r, action) {
		return
	}

	res := &Result{Success: true}
	if !viper.GetBool("standalone") {
		if !_hostCommand(w, r, action) {
			res.Success = false
			res.Message = "Command failed - see LabCA log for any details"
		}

		if action != "server-restart" && action != "version-update" {
			res.ManageComponents(w, r, action)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

func _manageGet(w http.ResponseWriter, r *http.Request) {
	manageData := make(map[string]interface{})
	manageData["RequestBase"] = r.Header.Get("X-Request-Base")

	if viper.GetBool("standalone") {
		manageData["Standalone"] = true
		manageData["Backend"] = viper.GetString("backend")

		dsn := strings.Split(viper.GetString("db.conn"), "@")
		if len(dsn) > 0 {
			up := strings.Split(dsn[0], ":")
			if len(up) > 0 {
				manageData["MySQLUser"] = up[0]
			}
			if len(up) > 1 {
				manageData["MySQLPasswd"] = up[1]
			}
		}
		if len(dsn) > 1 {
			sd := strings.Split(dsn[1], "/")
			if len(sd) > 0 {
				if strings.HasPrefix(sd[0], "tcp(") {
					sd[0] = sd[0][4 : len(sd[0])-1]
				}
				sp := strings.Split(sd[0], ":")
				if len(sp) > 0 {
					manageData["MySQLServer"] = sp[0]
				}
				if len(sp) > 1 {
					manageData["MySQLPort"] = sp[1]
				}
			}
			if len(sd) > 1 {
				manageData["MySQLDBName"] = sd[1]
			}
		}

		manageData["UseHTTPS"] = viper.GetBool("server.https")
		manageData["CertPath"] = viper.GetString("server.cert")
		manageData["KeyPath"] = viper.GetString("server.key")

	} else {
		checkUpdates(false)
		manageData["UpdateAvailable"] = updateAvailable
		manageData["UpdateChecked"] = strings.Replace(updateChecked.Format("02-Jan-2006 15:04:05 MST"), "+0000", "GMT", -1)
		manageData["UpdateCheckedRel"] = humanize.RelTime(updateChecked, time.Now(), "", "")

		components := _parseComponents(getLog(w, r, "components"))
		for i := 0; i < len(components); i++ {
			if components[i].Name == "Boulder (ACME)" {
				components[i].LogURL = r.Header.Get("X-Request-Base") + "/logs/boulder"
				components[i].LogTitle = "ACME Log"

				btn := make(map[string]interface{})
				cls := "btn-success"
				if components[i].TimestampRel != "stopped" {
					cls = cls + " hidden"
				}
				btn["Class"] = cls
				btn["Id"] = "boulder-start"
				btn["Title"] = "Start the core ACME application"
				btn["Label"] = "Start"
				components[i].Buttons = append(components[i].Buttons, btn)

				btn = make(map[string]interface{})
				cls = "btn-warning"
				if components[i].TimestampRel == "stopped" {
					cls = cls + " hidden"
				}
				btn["Class"] = cls
				btn["Id"] = "boulder-restart"
				btn["Title"] = "Stop and restart the core ACME application"
				btn["Label"] = "Restart"
				components[i].Buttons = append(components[i].Buttons, btn)

				btn = make(map[string]interface{})
				cls = "btn-danger"
				if components[i].TimestampRel == "stopped" {
					cls = cls + " hidden"
				}
				btn["Class"] = cls
				btn["Id"] = "boulder-stop"
				btn["Title"] = "Stop the core ACME application; users can no longer use ACME clients to interact with this instance"
				btn["Label"] = "Stop"
				components[i].Buttons = append(components[i].Buttons, btn)
			}

			if components[i].Name == "LabCA Controller" {
				components[i].LogURL = ""
				components[i].LogTitle = ""

				btn := make(map[string]interface{})
				btn["Class"] = "btn-warning"
				btn["Id"] = "svc-restart"
				btn["Title"] = "Restart the host service"
				btn["Label"] = "Restart"
				components[i].Buttons = append(components[i].Buttons, btn)
			}

			if components[i].Name == "LabCA Application" {
				components[i].LogURL = r.Header.Get("X-Request-Base") + "/logs/labca"
				components[i].LogTitle = "LabCA Log"

				btn := make(map[string]interface{})
				btn["Class"] = "btn-warning"
				btn["Id"] = "labca-restart"
				btn["Title"] = "Stop and restart this LabCA admin application"
				btn["Label"] = "Restart"
				components[i].Buttons = append(components[i].Buttons, btn)
			}

			if components[i].Name == "NGINX Webserver" {
				components[i].LogURL = r.Header.Get("X-Request-Base") + "/logs/web"
				components[i].LogTitle = "Web Error Log"

				btn := make(map[string]interface{})
				btn["Class"] = "btn-info"
				btn["Id"] = "nginx-reload"
				btn["Title"] = "Reload web server configuration with minimal impact to the users"
				btn["Label"] = "Reload"
				components[i].Buttons = append(components[i].Buttons, btn)

				btn = make(map[string]interface{})
				btn["Class"] = "btn-warning"
				btn["Id"] = "nginx-restart"
				btn["Title"] = "Restart the web server with some downtime for the users"
				btn["Label"] = "Restart"
				components[i].Buttons = append(components[i].Buttons, btn)
			}

			if components[i].Name == "Consul (Boulder)" {
				components[i].LogURL = ""
				components[i].LogTitle = ""

				btn := make(map[string]interface{})
				btn["Class"] = "btn-warning"
				btn["Id"] = "consul-restart"
				btn["Title"] = "Restart the Consul internal DNS helper"
				btn["Label"] = "Restart"
				components[i].Buttons = append(components[i].Buttons, btn)
			}

			if components[i].Name == "MySQL Database" {
				components[i].LogURL = ""
				components[i].LogTitle = ""

				btn := make(map[string]interface{})
				btn["Class"] = "btn-warning"
				btn["Id"] = "mysql-restart"
				btn["Title"] = "Restart the MySQL database server"
				btn["Label"] = "Restart"
				components[i].Buttons = append(components[i].Buttons, btn)
			}
		}
		manageData["Components"] = components

		backupFiles := strings.Split(getLog(w, r, "backups"), "\n")
		backupFiles = backupFiles[:len(backupFiles)-1]
		manageData["BackupFiles"] = backupFiles

		manageData["RootDetails"] = _doCmdOutput(w, r, "openssl x509 -noout -text -nameopt utf8 -in data/root-ca.pem")
		manageData["IssuerDetails"] = _doCmdOutput(w, r, "openssl x509 -noout -text -nameopt utf8 -in data/issuer/ca-int.pem")

		if viper.Get("crl_interval") == nil || viper.GetString("crl_interval") == "" {
			manageData["CRLInterval"] = "24h"
		} else {
			ci := strings.Split(viper.GetString("crl_interval"), "|")
			manageData["CRLInterval"] = ci[0]
		}

		manageData["Fqdn"] = viper.GetString("labca.fqdn")
		manageData["Organization"] = viper.GetString("labca.organization")
		if viper.Get("labca.web_title") == nil || viper.GetString("labca.web_title") == "" {
			manageData["WebTitle"] = "LabCA"
		} else {
			manageData["WebTitle"] = viper.GetString("labca.web_title")
		}
		manageData["DNS"] = viper.GetString("labca.dns")
		domainMode := viper.GetString("labca.domain_mode")
		manageData["DomainMode"] = domainMode
		if domainMode == "lockdown" {
			manageData["LockdownDomains"] = viper.GetString("labca.lockdown")
		}
		if domainMode == "whitelist" {
			manageData["WhitelistDomains"] = viper.GetString("labca.whitelist")
		}
		manageData["ExtendedTimeout"] = viper.GetBool("labca.extended_timeout")

		manageData["DoEmail"] = viper.GetBool("labca.email.enable")
		manageData["Server"] = viper.GetString("labca.email.server")
		manageData["Port"] = viper.GetInt("labca.email.port")
		manageData["EmailUser"] = viper.GetString("labca.email.user")
		manageData["EmailPwd"] = ""
		if viper.Get("labca.email.pass") != nil {
			pwd := viper.GetString("labca.email.pass")
			result, err := _decrypt(pwd)
			if err == nil {
				manageData["EmailPwd"] = string(result)
			} else {
				log.Printf("WARNING: could not decrypt email password: %s!\n", err.Error())
			}
		}
		manageData["From"] = viper.GetString("labca.email.from")
	}

	manageData["Name"] = viper.GetString("user.name")
	manageData["Email"] = viper.GetString("user.email")

	manageData["Title"] = "Manage"

	render(w, r, "manage", manageData)
}

func manageHandler(w http.ResponseWriter, r *http.Request) {
	if !viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	if r.Method == "POST" {
		_managePost(w, r)
	} else {
		_manageGet(w, r)
	}
}

func logsHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	logType := vars["type"]

	proto := "ws"
	if r.Header.Get("X-Forwarded-Proto") == "https" {
		proto = "wss"
	}

	wsurl := proto + "://" + r.Host + r.Header.Get("X-Request-Base") + "/ws?logType=" + logType

	var name string
	var message string
	var data string

	switch logType {
	case "cert":
		name = "Web Certificate Log"
		message = "Log file for the certificate renewal for this server."
		wsurl = ""
		data = getLog(w, r, logType)
	case "boulder":
		name = "ACME Backend Log"
		message = "Live view on the backend ACME application (Boulder) logs."
	case "audit":
		name = "ACME Audit Log"
		message = "Live view on only the audit messages in the backend ACME application (Boulder) logs."
	case "cron":
		name = "Cron Log"
		message = "Live view on the logs for the cron jobs for LabCA."
	case "labca":
		name = "LabCA Log"
		message = "Live view on the logs for this LabCA web application."
	case "web":
		name = "Web Access Log"
		message = "Live view on the NGINX web server access log."
	default:
		errorHandler(w, r, fmt.Errorf("unknown log type '%s'", logType), http.StatusBadRequest)
		return
	}

	render(w, r, "logs", map[string]interface{}{
		"Name":    name,
		"Message": message,
		"Data":    data,
		"WsUrl":   wsurl,
		"Title":   "Logs",
	})
}

func getLog(w http.ResponseWriter, r *http.Request, logType string) string {
	conn, err := net.Dial("tcp", "control:3030")
	if err != nil {
		_, _ = exeCmd("sleep 5")
		errorHandler(w, r, err, http.StatusInternalServerError)
		return ""
	}

	defer conn.Close()

	fmt.Fprintf(conn, "log-"+logType+"\n")
	reader := bufio.NewReader(conn)
	contents, err := io.ReadAll(reader)
	if err != nil {
		errorHandler(w, r, err, http.StatusInternalServerError)
		return ""
	}

	return string(contents)
}

func wsErrorHandler(err error) {
	log.Printf("wsErrorHandler: %v", err)

	pc := make([]uintptr, 15)
	n := runtime.Callers(2, pc)
	frames := runtime.CallersFrames(pc[:n])
	frame, _ := frames.Next()
	fmt.Printf("%s:%d, %s\n", frame.File, frame.Line, frame.Function)

	debug.PrintStack()
}

func showLog(ws *websocket.Conn, logType string) {
	conn, err := net.Dial("tcp", "control:3030")
	if err != nil {
		_, _ = exeCmd("sleep 5")
		wsErrorHandler(err)
		return
	}

	defer conn.Close()

	fmt.Fprintf(conn, "log-"+logType+"\n")
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		msg := scanner.Text()
		if logType != "audit" || strings.Contains(msg, "[AUDIT]") {
			ws.SetWriteDeadline(time.Now().Add(writeWait))
			if err := ws.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
				// Probably "websocket: close sent"
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		wsErrorHandler(err)
		return
	}
}

func reader(ws *websocket.Conn) {
	defer ws.Close()
	ws.SetReadLimit(512)
	ws.SetReadDeadline(time.Now().Add(pongWait))
	ws.SetPongHandler(func(string) error { ws.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, _, err := ws.ReadMessage()
		if err != nil {
			break
		}
	}
}

func writer(ws *websocket.Conn, logType string) {
	pingTicker := time.NewTicker(pingPeriod)
	defer func() {
		pingTicker.Stop()
		ws.Close()
	}()

	go showLog(ws, logType)

	for range pingTicker.C {
		ws.SetWriteDeadline(time.Now().Add(writeWait))
		if err := ws.WriteMessage(websocket.PingMessage, []byte{}); err != nil {
			// Probably "websocket: close sent"
			return
		}
	}
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		if _, ok := err.(websocket.HandshakeError); !ok {
			log.Println(err)
		}
		return
	}

	logType := r.FormValue("logType")

	switch logType {
	case "boulder":
	case "audit":
	case "labca":
	case "cron":
	case "web":
	default:
		errorHandler(w, r, fmt.Errorf("unknown log type '%s'", logType), http.StatusBadRequest)
		return
	}

	go writer(ws, logType)
	reader(ws)
}

func _buildCI(r *http.Request, session *sessions.Session, isRoot bool) *CertificateInfo {
	ci := &CertificateInfo{
		IsRoot:      isRoot,
		CreateType:  "generate",
		CommonName:  "Root CA",
		RequestBase: r.Header.Get("X-Request-Base"),
	}
	if !isRoot {
		ci.CommonName = "CA"
	}
	ci.Initialize()

	if session.Values["ct"] != nil {
		if !isRoot && session.Values["ct"].(string) == "generate" {
			ci.IsRootGenerated = true
		}
		ci.CreateType = session.Values["ct"].(string)
	}
	if session.Values["kt"] != nil {
		ci.KeyType = session.Values["kt"].(string)
	}
	if session.Values["c"] != nil {
		ci.Country = session.Values["c"].(string)
	}
	if session.Values["o"] != nil {
		ci.Organization = session.Values["o"].(string)
	}
	if session.Values["ou"] != nil {
		ci.OrgUnit = session.Values["ou"].(string)
	}
	if session.Values["cn"] != nil {
		ci.CommonName = session.Values["cn"].(string)
		ci.CommonName = strings.Replace(ci.CommonName, "Root", "", -1)
		ci.CommonName = strings.Replace(ci.CommonName, "  ", " ", -1)
	}

	return ci
}

func issuerNameID(certfile string) (int64, error) {
	cf, err := os.ReadFile(certfile)
	if err != nil {
		log.Printf("issuerNameID: could not read cert file: %v", err)
		return 0, err
	}

	cpb, _ := pem.Decode(cf)
	crt, err := x509.ParseCertificate(cpb.Bytes)
	if err != nil {
		log.Printf("issuerNameID: could not parse x509 file: %v", err)
		return 0, err
	}

	// From issuance/issuance.go : func truncatedHash
	h := crypto.SHA1.New()
	h.Write(crt.RawSubject)
	s := h.Sum(nil)
	return int64(big.NewInt(0).SetBytes(s[:7]).Int64()), nil
}

func _certCreate(w http.ResponseWriter, r *http.Request, certBase string, isRoot bool) bool {
	if r.Method == "POST" {
		if err := r.ParseMultipartForm(2 * 1024 * 1024); err != nil {
			errorHandler(w, r, err, http.StatusInternalServerError)
			return false
		}

		if r.Form.Get("revertroot") != "" {
			// From issuer certificate creation page it is possible to remove the root again and start over
			exeCmd("rm data/root-ca.key")     // Does not necessarily exist
			exeCmd("rm data/root-ca.key.der") // Does not necessarily exist
			if _, err := exeCmd("rm data/root-ca.pem"); err != nil {
				errorHandler(w, r, err, http.StatusInternalServerError)
				return false
			}
			certBase = "root-ca"
			isRoot = true
			r.Method = "GET"
			sess, _ := sessionStore.Get(r, "labca")
			sess.Values["ct"] = "generate"
			if err := sess.Save(r, w); err != nil {
				log.Printf("cannot save session: %s\n", err)
			}
		} else if r.Form.Get("ack-rootkey") == "yes" {
			// Root Key was shown, do we need to keep it online?
			viper.Set("keep_root_offline", r.Form.Get("keep-root-online") != "true")
			viper.WriteConfig()

			// Undo what setupHandler did when showing the public key...
			_, errPem := os.Stat("data/root-ca.pem")
			_, errTmp := os.Stat("data/root-ca.pem_TMP")
			if os.IsNotExist(errPem) && !os.IsNotExist(errTmp) {
				exeCmd("mv data/root-ca.pem_TMP data/root-ca.pem")
			}

			r.Method = "GET"
			return true
		}
	}

	path := "data/"
	if !isRoot {
		path = path + "issuer/"
	}

	if _, err := os.Stat(path + certBase + ".pem"); os.IsNotExist(err) {
		session, _ := sessionStore.Get(r, "labca")

		if r.Method == "GET" {
			ci := _buildCI(r, session, isRoot)

			render(w, r, "cert:manage", map[string]interface{}{"CertificateInfo": ci, "Progress": _progress(certBase), "HelpText": _helptext(certBase)})
			return false
		} else if r.Method == "POST" {
			if err := r.ParseMultipartForm(2 * 1024 * 1024); err != nil {
				errorHandler(w, r, err, http.StatusInternalServerError)
				return false
			}

			ci := &CertificateInfo{
				IsRoot: r.Form.Get("cert") == "root",
			}
			ci.Initialize()
			ci.IsRoot = r.Form.Get("cert") == "root"
			ci.CreateType = r.Form.Get("createtype")

			if r.Form.Get("keytype") != "" {
				ci.KeyType = r.Form.Get("keytype")
			}
			ci.Country = r.Form.Get("c")
			ci.Organization = r.Form.Get("o")
			ci.OrgUnit = r.Form.Get("ou")
			ci.CommonName = r.Form.Get("cn")

			if ci.CreateType == "import" {
				file, handler, err := r.FormFile("import")
				if err != nil {
					errorHandler(w, r, err, http.StatusInternalServerError)
					return false
				}

				defer file.Close()

				ci.ImportFile = file
				ci.ImportHandler = handler
				ci.ImportPwd = r.Form.Get("import-pwd")
			}

			ci.Key = r.Form.Get("key")
			ci.Passphrase = r.Form.Get("passphrase")
			ci.Certificate = r.Form.Get("certificate")
			ci.RequestBase = r.Header.Get("X-Request-Base")

			if !ci.Validate() {
				if session.Values["csr"] == true {
					delete(ci.Errors, "Key")
				} else {
					render(w, r, "cert:manage", map[string]interface{}{"CertificateInfo": ci, "Progress": _progress(certBase), "HelpText": _helptext(certBase)})
					return false
				}
			}

			wasCSR := session.Values["csr"] == true
			if r.Form.Get("ack-rootkey") != "yes" {
				if r.Form.Get("rootkey") != "" {
					rootci := &CertificateInfo{
						IsRoot:     true,
						Key:        r.Form.Get("rootkey"),
						Passphrase: r.Form.Get("rootpassphrase"),
					}
					if !rootci.StoreRootKey("data/") {
						ci.Errors["Modal"] = rootci.Errors["Modal"]
						render(w, r, "cert:manage", map[string]interface{}{"CertificateInfo": ci, "GetRootKey": true, "Progress": _progress(certBase), "HelpText": _helptext(certBase)})
						return false
					}
				}
				if r.Form.Get("crl") != "" {
					rootci := &CertificateInfo{
						IsRoot: true,
						CRL:    r.Form.Get("crl"),
					}
					if !rootci.StoreCRL("data/") {
						ci.Errors["Modal"] = rootci.Errors["Modal"]
						csr, err := os.Open(path + certBase + ".csr")
						if err != nil {
							ci.Errors[cases.Title(language.Und).String(ci.CreateType)] = "Error reading .csr file! See LabCA logs for details"
							log.Printf("_certCreate: read csr: %v", err)
							render(w, r, "cert:manage", map[string]interface{}{"CertificateInfo": ci, "Progress": _progress(certBase), "HelpText": _helptext(certBase)})
							return false
						}
						defer csr.Close()
						b, err := ioutil.ReadAll(csr)

						render(w, r, "cert:manage", map[string]interface{}{"CertificateInfo": ci, "CSR": string(b), "Progress": _progress(certBase), "HelpText": _helptext(certBase)})
						return false
					}
				}

				if err := ci.Create(path, certBase, wasCSR); err != nil {
					if err.Error() == "NO_ROOT_KEY" {
						if r.Form.Get("generate") != "" {
							if r.Form.Get("rootkey") == "" {
								render(w, r, "cert:manage", map[string]interface{}{"CertificateInfo": ci, "GetRootKey": true, "Progress": _progress(certBase), "HelpText": _helptext(certBase)})
								return false
							} else {
								rootci := &CertificateInfo{
									IsRoot:     true,
									Key:        r.Form.Get("rootkey"),
									Passphrase: r.Form.Get("rootpassphrase"),
								}
								if !rootci.StoreRootKey("data/") {
									ci.Errors["Modal"] = rootci.Errors["Modal"]
									render(w, r, "cert:manage", map[string]interface{}{"CertificateInfo": ci, "GetRootKey": true, "Progress": _progress(certBase), "HelpText": _helptext(certBase)})
									return false
								}

								render(w, r, "cert:manage", map[string]interface{}{"CertificateInfo": ci, "Progress": _progress(certBase), "HelpText": _helptext(certBase)})
								return false
							}
						}

						if r.Form.Get("getcsr") != "" {
							csr, err := os.Open(path + certBase + ".csr")
							if err != nil {
								ci.Errors[cases.Title(language.Und).String(ci.CreateType)] = "Error reading .csr file! See LabCA logs for details"
								log.Printf("_certCreate: read csr: %v", err)
								render(w, r, "cert:manage", map[string]interface{}{"CertificateInfo": ci, "Progress": _progress(certBase), "HelpText": _helptext(certBase)})
								return false
							}
							defer csr.Close()
							b, err := ioutil.ReadAll(csr)

							session.Values["csr"] = true
							if err = session.Save(r, w); err != nil {
								log.Printf("cannot save session: %s\n", err)
							}

							render(w, r, "cert:manage", map[string]interface{}{"CertificateInfo": ci, "CSR": string(b), "Progress": _progress(certBase), "HelpText": _helptext(certBase)})
							return false
						}
					} else {
						ci.Errors[cases.Title(language.Und).String(ci.CreateType)] = err.Error()
						log.Printf("_certCreate: create failed: %v", err)
						render(w, r, "cert:manage", map[string]interface{}{"CertificateInfo": ci, "Progress": _progress(certBase), "HelpText": _helptext(certBase)})
						return false
					}
				}
			}

			if !ci.IsRoot {
				nameID, err := issuerNameID(path + certBase + ".pem")
				if err == nil {
					viper.Set("issuer_name_id", nameID)
					viper.WriteConfig()
				} else {
					log.Printf("_certCreate: could not calculate IssuerNameID: %v", err)
				}

				if viper.GetBool("keep_root_offline") {
					if _, err := os.Stat(path + "../root-ca.key"); !os.IsNotExist(err) {
						fmt.Println("Removing private Root key from the system...")
						if _, err := exeCmd("rm " + path + "../root-ca.key"); err != nil {
							log.Printf("_certCreate: error deleting root key: %v", err)
						}
					}
					if _, err := os.Stat(path + "../root-ca.key.der"); !os.IsNotExist(err) {
						if _, err := exeCmd("rm " + path + "../root-ca.key.der"); err != nil {
							log.Printf("_certCreate: error deleting root key (DER format): %v", err)
						}
					}
				}
			}

			if viper.Get("labca.organization") == nil {
				viper.Set("labca.organization", ci.Organization)
				viper.WriteConfig()
			}

			session.Values["ct"] = ci.CreateType
			session.Values["kt"] = ci.KeyType
			session.Values["c"] = ci.Country
			session.Values["o"] = ci.Organization
			session.Values["ou"] = ci.OrgUnit
			session.Values["cn"] = ci.CommonName
			if err = session.Save(r, w); err != nil {
				log.Printf("cannot save session: %s\n", err)
			}

			if ci.IsRoot && ci.CreateType == "generate" && r.Form.Get("ack-rootkey") != "yes" {
				key, err := os.Open(path + certBase + ".key")
				if err != nil {
					ci.Errors[cases.Title(language.Und).String(ci.CreateType)] = "Error reading .key file! See LabCA logs for details"
					log.Printf("_certCreate: read key: %v", err)
					render(w, r, "cert:manage", map[string]interface{}{"CertificateInfo": ci, "Progress": _progress(certBase), "HelpText": _helptext(certBase)})
					return false
				}
				defer key.Close()
				b, err := ioutil.ReadAll(key)

				render(w, r, "cert:manage", map[string]interface{}{"CertificateInfo": ci, "RootKey": string(b), "Progress": _progress(certBase), "HelpText": _helptext(certBase)})
				return false
			}

			// Fake the method to GET as we need to continue in the setupHandler() function
			r.Method = "GET"
		} else {
			http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusSeeOther)
			return false
		}
	}

	return true
}

func _hostCommand(w http.ResponseWriter, r *http.Request, command string, params ...string) bool {
	conn, err := net.Dial("tcp", "control:3030")
	if err != nil {
		_, _ = exeCmd("sleep 5")
		errorHandler(w, r, err, http.StatusInternalServerError)
		return false
	}

	defer conn.Close()

	fmt.Fprintf(conn, command+"\n")
	for _, param := range params {
		fmt.Fprintf(conn, param+"\n")
	}

	reader := bufio.NewReader(conn)
	message, err := io.ReadAll(reader)
	if err != nil {
		errorHandler(w, r, err, http.StatusInternalServerError)
		return false
	}

	if strings.Compare(string(message), "ok\n") == 0 {
		return true
	}

	if len(message) >= 4 {
		tail := message[len(message)-4:]
		if strings.Compare(string(tail), "\nok\n") == 0 {
			msg := message[0 : len(message)-4]
			log.Printf("Message from server: '%s'", msg)
			return true
		}
	}

	log.Printf("ERROR: Message from server: '%s'", message)
	errorHandler(w, r, errors.New(string(message)), http.StatusInternalServerError)
	return false
}

func randToken() string {
	b := make([]byte, 8)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func _applyConfig() error {
	_, err := exeCmd("./apply")
	if err != nil {
		fmt.Println("")
	}
	return err
}

func _progress(stage string) int {
	max := 20.0 / 100.0
	curr := 1.0

	if stage == "register" {
		return int(math.Round(curr / max))
	}
	curr += 2.0

	if stage == "setup" {
		return int(math.Round(curr / max))
	}
	curr += 3.0

	if stage == "root-ca" {
		return int(math.Round(curr / max))
	}
	curr += 4.0

	if stage == "ca-int" {
		return int(math.Round(curr / max))
	}
	curr += 3.0

	if stage == "polling" {
		return int(math.Round(curr / max))
	}
	curr += 4.0

	if stage == "wrapup" {
		return int(math.Round(curr / max))
	}
	curr += 3.0

	if stage == "final" {
		return int(math.Round(curr / max))
	}

	if stage == "standalone" {
		return int(math.Round(0.6 * curr / max))
	}

	return 0
}

func _helptext(stage string) template.HTML {
	if stage == "register" {
		return template.HTML(fmt.Sprint("<p class=\"form-register\">You need to create an admin account for\n",
			"managing this instance of LabCA. There can only be one admin account, but you can configure all\n",
			"its attributes once the initial setup has completed.<br><br>Instead, you can also\n",
			"<a href=\"#\" onclick=\"false\" class=\"toggle-restore\">restore from a backup file</a> of a\n",
			"previous LabCA installation.</p>\n",
			"<p class=\"form-restore\">If you have a backup file from a previous LabCA installation and want to\n",
			"restore this instance with the exact same configuration, use that backup file here.\n",
			"<br><br>Otherwise you should follow the <a href=\"#\" onclick=\"false\"\n",
			"class=\"toggle-register\">standard setup</a>.</p>"))
	} else if stage == "setup" {
		return template.HTML(fmt.Sprint("<p>The fully qualified domain name (FQDN) is what end users will use\n",
			"to connect to this server. It was provided in the initial setup and is shown here for reference.</p>\n",
			"<p>Please fill in a DNS server (and optionally port, default is ':53') that will be used to lookup\n",
			"the domains for which a certificate is requested.</p>\n",
			"<p>LabCA is primarily intended for use inside an organization where all domains end in the same\n",
			"domain, e.g. '.localdomain'. In lockdown mode only those domains are allowed. In whitelist mode\n",
			"those domains are allowed next to all official, internet accessible domains and in standard\n",
			"mode only the official domains are allowed.</p>"))
	} else if stage == "root-ca" {
		return template.HTML(fmt.Sprint("<p>This is the top level certificate that will sign the issuer\n",
			"certificate(s). You can either generate a fresh Root CA (Certificate Authority) or import an\n",
			"existing one, e.g. a backup from another LabCA instance.</p>\n",
			"<p>If you want to <b>generate</b> a new certificate, pick a key type and strength (the higher the number the\n",
			"more secure, ECDSA is more modern than RSA), provide at least a country and organization name,\n",
			"and the common name. It is recommended that the common name contains the word 'Root' as well\n",
			"as your organization name so you can recognize it, and that's why that is automatically filled\n",
			"once you leave the organization field.</p>\n",
			"<p>If you want to <b>upload</b> an existing root certificate, you may choose to keep the private key\n",
			"offline for security reasons according to best practices. If you do include it here, we will be able\n",
			"to generate an issuing certificate automatically in the next step. If you don't include it, we will\n",
			"ask for it when needed.</p>"))
	} else if stage == "ca-int" {
		return template.HTML(fmt.Sprint("<p>This is what end users will see as the issuing certificate. Again,\n",
			"you can either generate a fresh certificate or import an existing one, as long as it is signed by\n",
			"the Root CA from the previous step.</p>\n",
			"<p>If you want to <b>generate</b> a certificate, by default the same key type and strength is selected as\n",
			"was chosen in the previous step when generating the root, but you may choose a different\n",
			"one. By default the common name is the same as the CN for the Root CA, minus the word 'Root'.</p>\n",
			"<p>If you are using an offline Root CA certificate then you can download the Certificate Signing\n",
			"Request (CSR) here and have it signed by the Root CA. Alternatively we (temporarily) need the\n",
			"secret key of the Root CA for generating the issuing certificate.</p>"))
	} else if stage == "standalone" {
		return template.HTML(fmt.Sprint("<p>Currently only step-ca is supported, using the MySQL database backend.\n",
			"Please provide the necessary connectiuon details here."))
	} else {
		return template.HTML("")
	}
}

func _setupAdminUser(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == "GET" {
		reg := &User{
			RequestBase: r.Header.Get("X-Request-Base"),
		}
		render(w, r, "register:manage", map[string]interface{}{"User": reg, "IsLogin": true, "Progress": _progress("register"), "HelpText": _helptext("register")})
		return false
	} else if r.Method == "POST" {
		isMultipart := true
		if err := r.ParseMultipartForm(1 * 1024 * 1024); err != nil {
			isMultipart = false
			if err != http.ErrNotMultipart {
				errorHandler(w, r, err, http.StatusInternalServerError)
				return false
			} else if err := r.ParseForm(); err != nil {
				errorHandler(w, r, err, http.StatusInternalServerError)
				return false
			}
		}

		// Restore a backup file
		if isMultipart {
			reg := &User{
				Errors:      make(map[string]string),
				RequestBase: r.Header.Get("X-Request-Base"),
			}
			file, header, err := r.FormFile("file")
			if err != nil {
				fmt.Println(err)
				reg.Errors["File"] = "Could not read uploaded file"
				render(w, r, "register:manage", map[string]interface{}{"User": reg, "IsLogin": true, "Progress": _progress("register"), "HelpText": _helptext("register")})
				return false
			}
			defer file.Close()

			out, err := os.Create("/opt/backup/" + header.Filename)
			if err != nil {
				fmt.Println(err)
				reg.Errors["File"] = "Could not create local file"
				render(w, r, "register:manage", map[string]interface{}{"User": reg, "IsLogin": true, "Progress": _progress("register"), "HelpText": _helptext("register")})
				return false
			}
			defer out.Close()

			_, copyError := io.Copy(out, file)
			if copyError != nil {
				fmt.Println(err)
				reg.Errors["File"] = "Could not store uploaded file"
				render(w, r, "register:manage", map[string]interface{}{"User": reg, "IsLogin": true, "Progress": _progress("register"), "HelpText": _helptext("register")})
				return false
			}

			// Cannot use _hostCommand() as we need different error handling
			conn, err := net.Dial("tcp", "control:3030")
			if err != nil {
				fmt.Println(err)
				reg.Errors["File"] = "Could not import backup file: error communicating with control"
				render(w, r, "register:manage", map[string]interface{}{"User": reg, "IsLogin": true, "Progress": _progress("register"), "HelpText": _helptext("register")})
				return false
			}
			defer conn.Close()

			fmt.Fprintf(conn, "backup-restore\n"+header.Filename+"\n")
			reader := bufio.NewReader(conn)
			message, err := io.ReadAll(reader)
			if err != nil {
				fmt.Println(err)
				reg.Errors["File"] = "Could not import backup file: error reading control response"
				render(w, r, "register:manage", map[string]interface{}{"User": reg, "IsLogin": true, "Progress": _progress("register"), "HelpText": _helptext("register")})
				return false
			}

			if strings.Compare(string(message), "ok\n") == 0 {
				if err := viper.ReadInConfig(); err != nil {
					fmt.Println(err)
					reg.Errors["File"] = "Could not read config after importing backup"
					render(w, r, "register:manage", map[string]interface{}{"User": reg, "IsLogin": true, "Progress": _progress("register"), "HelpText": _helptext("register")})
					return false
				}

				viper.Set("config.complete", false)
				viper.WriteConfig()

				err = _applyConfig()
				if err != nil {
					fmt.Println(err)
					reg.Errors["File"] = "Could not apply config after importing backup"
					render(w, r, "register:manage", map[string]interface{}{"User": reg, "IsLogin": true, "Progress": _progress("register"), "HelpText": _helptext("register")})
					return false
				}

				defer _hostCommand(w, r, "docker-restart")
				http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/final", http.StatusFound)
				return true
			}

			if len(message) >= 4 {
				tail := message[len(message)-4:]
				if strings.Compare(string(tail), "\nok\n") == 0 {
					msg := message[0 : len(message)-4]
					log.Printf("Message from server: '%s'", msg)
					lines := strings.Split(strings.TrimSpace(string(msg)), "\n")
					reg.Errors["File"] = "Could not import backup file: " + lines[0]
					render(w, r, "register:manage", map[string]interface{}{"User": reg, "IsLogin": true, "Progress": _progress("register"), "HelpText": _helptext("register")})
					return false
				}
			}

			log.Printf("ERROR: Message from server: '%s'", message)
			lines := strings.Split(strings.TrimSpace(string(message)), "\n")
			reg.Errors["File"] = "Could not import backup file: " + lines[0]
			render(w, r, "register:manage", map[string]interface{}{"User": reg, "IsLogin": true, "Progress": _progress("register"), "HelpText": _helptext("register")})
			return false
		}

		// Regular setup form handling
		reg := &User{
			Name:        r.Form.Get("username"),
			Email:       r.Form.Get("email"),
			Password:    r.Form.Get("password"),
			Confirm:     r.Form.Get("confirm"),
			RequestBase: r.Header.Get("X-Request-Base"),
		}

		if !reg.Validate(true, false) {
			render(w, r, "register:manage", map[string]interface{}{"User": reg, "IsLogin": true, "Progress": _progress("register"), "HelpText": _helptext("register")})
			return false
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(reg.Password), bcrypt.MinCost)
		if err != nil {
			errorHandler(w, r, err, http.StatusInternalServerError)
			return false
		}
		viper.Set("user.name", reg.Name)
		viper.Set("user.email", reg.Email)
		viper.Set("user.password", string(hash))
		viper.WriteConfig()

		session, _ := sessionStore.Get(r, "labca")
		session.Values["user"] = reg.Name
		if err = session.Save(r, w); err != nil {
			log.Printf("cannot save session: %s\n", err)
		}

		// Fake the method to GET as we need to continue in the setupHandler() function
		r.Method = "GET"
	} else {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusSeeOther)
		return false
	}

	return true
}

func _setupBaseConfig(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == "GET" {
		domain := viper.GetString("labca.fqdn")
		pos := strings.Index(domain, ".")
		if pos > -1 {
			pos = pos + 1
			domain = domain[pos:]
		}

		cfg := &SetupConfig{
			Fqdn:             viper.GetString("labca.fqdn"),
			DomainMode:       "lockdown",
			LockdownDomains:  domain,
			WhitelistDomains: domain,
			RequestBase:      r.Header.Get("X-Request-Base"),
		}

		render(w, r, "setup:manage", map[string]interface{}{"SetupConfig": cfg, "Progress": _progress("setup"), "HelpText": _helptext("setup")})
		return false
	} else if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			errorHandler(w, r, err, http.StatusInternalServerError)
			return false
		}

		cfg := &SetupConfig{
			Fqdn:             r.Form.Get("fqdn"),
			DNS:              r.Form.Get("dns"),
			DomainMode:       r.Form.Get("domain_mode"),
			LockdownDomains:  r.Form.Get("lockdown_domains"),
			WhitelistDomains: r.Form.Get("whitelist_domains"),
			RequestBase:      r.Header.Get("X-Request-Base"),
		}

		if !cfg.Validate(false) {
			render(w, r, "setup:manage", map[string]interface{}{"SetupConfig": cfg, "Progress": _progress("setup"), "HelpText": _helptext("setup")})
			return false
		}

		matched, err := regexp.MatchString(":\\d+$", cfg.DNS)
		if err == nil && !matched {
			cfg.DNS += ":53"
		}

		viper.Set("labca.fqdn", cfg.Fqdn)
		viper.Set("labca.dns", cfg.DNS)
		viper.Set("labca.domain_mode", cfg.DomainMode)
		if cfg.DomainMode == "lockdown" {
			viper.Set("labca.lockdown", cfg.LockdownDomains)
		}
		if cfg.DomainMode == "whitelist" {
			viper.Set("labca.whitelist", cfg.WhitelistDomains)
		}
		viper.WriteConfig()

		// Fake the method to GET as we need to continue in the setupHandler() function
		r.Method = "GET"
	} else {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusSeeOther)
		return false
	}

	return true
}

func writeStandaloneConfig(cfg *StandaloneConfig) {
	conn := cfg.MySQLUser
	if cfg.MySQLPasswd != "" {
		conn += ":" + cfg.MySQLPasswd
	}
	conn += "@"
	_, err := strconv.Atoi(string(strings.TrimSpace(cfg.MySQLServer)[0]))
	if err == nil {
		conn += "tcp(" + cfg.MySQLServer + ":" + cfg.MySQLPort + ")"
	} else {
		conn += cfg.MySQLServer + ":" + cfg.MySQLPort
	}
	conn += "/" + cfg.MySQLDBName

	restart := viper.GetBool("server.https") != cfg.UseHTTPS || viper.GetString("server.cert") != cfg.CertPath || viper.GetString("server.key") != cfg.KeyPath
	dbConn = conn
	viper.Set("db.conn", conn)
	viper.Set("backend", cfg.Backend)
	viper.Set("server.https", cfg.UseHTTPS)
	if cfg.UseHTTPS {
		viper.Set("server.cert", cfg.CertPath)
		viper.Set("server.key", cfg.KeyPath)
	}
	viper.Set("config.complete", true)
	viper.WriteConfig()

	if restart {
		if cfg.UseHTTPS {
			fmt.Println("### Please restart the application to use the HTTPS certificate!")
		} else {
			fmt.Println("### Please restart the application!")
		}
	}
}

func setupStandalone(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		cfg := &StandaloneConfig{
			RequestBase: r.Header.Get("X-Request-Base"),
			Backend:     "step-ca",
			MySQLServer: "127.0.0.1",
			MySQLPort:   "3306",
			MySQLDBName: "stepca",
			UseHTTPS:    false,
			CertPath:    configPath + string(os.PathSeparator) + "labca.crt",
			KeyPath:     configPath + string(os.PathSeparator) + "labca.key",
		}

		render(w, r, "standalone:manage", map[string]interface{}{"SetupConfig": cfg, "Progress": _progress("standalone"), "HelpText": _helptext("standalone")})
		return

	} else if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			errorHandler(w, r, err, http.StatusInternalServerError)
			return
		}

		cfg := &StandaloneConfig{
			Backend:     r.Form.Get("backend"),
			MySQLServer: r.Form.Get("mysql_server"),
			MySQLPort:   r.Form.Get("mysql_port"),
			MySQLDBName: r.Form.Get("mysql_dbname"),
			MySQLUser:   r.Form.Get("mysql_user"),
			MySQLPasswd: r.Form.Get("mysql_passwd"),
			UseHTTPS:    (r.Form.Get("use_https") == "https"),
			CertPath:    r.Form.Get("cert_path"),
			KeyPath:     r.Form.Get("key_path"),
			RequestBase: r.Header.Get("X-Request-Base"),
		}

		if !cfg.Validate() {
			render(w, r, "standalone:manage", map[string]interface{}{"SetupConfig": cfg, "Progress": _progress("standalone"), "HelpText": _helptext("standalone")})
			return
		}

		writeStandaloneConfig(cfg)

		// Fake the method to GET as we need to continue in the setupHandler() function
		r.Method = "GET"

	} else {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/", http.StatusFound)
}

func setupHandler(w http.ResponseWriter, r *http.Request) {
	if viper.GetBool("config.complete") {
		render(w, r, "index:manage", map[string]interface{}{"Message": template.HTML("Setup already completed! Go <a href=\"" + r.Header.Get("X-Request-Base") + "/\">home</a>")})
		return
	}

	// 1. Setup admin user
	if viper.Get("user.password") == nil {
		if !_setupAdminUser(w, r) {
			return
		}
	}

	// 1a. Go to standalone setup
	if viper.GetBool("standalone") {
		setupStandalone(w, r)
		return
	}

	// 2. Setup essential configuration
	if viper.Get("labca.dns") == nil {
		if !_setupBaseConfig(w, r) {
			return
		}
	}

	// 3. Setup root CA certificate
	if !_certCreate(w, r, "root-ca", true) {
		// Cleanup the cert (if it even exists) so we will retry on the next run
		if _, err := os.Stat("data/root-ca.pem"); !os.IsNotExist(err) {
			exeCmd("mv data/root-ca.pem data/root-ca.pem_TMP")
		}
		return
	}

	// 4. Setup issuer certificate
	if !_certCreate(w, r, "ca-int", false) {
		// Cleanup the cert (if it even exists) so we will retry on the next run
		os.Remove("data/issuer/ca-int.pem")
		return
	}

	// 5. Apply configuration / populate with certificate info
	err := _applyConfig()
	if err != nil {
		errorHandler(w, r, err, http.StatusInternalServerError)
		return
	}

	if !viper.GetBool("config.restarted") {
		// Don't let the retry mechanism generate new restartSecret!
		if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
			_, _ = exeCmd("sleep 5")
			render(w, r, "index", map[string]interface{}{"Message": "Retry OK"})
		} else {
			// 8. Restart application
			restartSecret = randToken()
			http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/wait?restart="+restartSecret, http.StatusFound)
		}
		return
	}

	render(w, r, "wrapup:manage", map[string]interface{}{"Progress": _progress("wrapup"), "HelpText": _helptext("wrapup")})
}

func waitHandler(w http.ResponseWriter, r *http.Request) {
	if viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/", http.StatusFound)
		return
	}

	render(w, r, "polling:manage", map[string]interface{}{"Progress": _progress("polling"), "HelpText": _helptext("polling")})
}

func restartHandler(w http.ResponseWriter, r *http.Request) {
	if viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/", http.StatusFound)
		return
	}

	if strings.Compare(r.URL.Query().Get("token"), restartSecret) != 0 {
		log.Println("WARNING: Restart token ('" + r.URL.Query().Get("token") + "') does not match our secret ('" + restartSecret + "')!")
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	viper.Set("config.restarted", true)
	viper.WriteConfig()

	if !_hostCommand(w, r, "docker-restart") {
		viper.Set("config.restarted", false)
		viper.WriteConfig()
		return
	}
}

func finalHandler(w http.ResponseWriter, r *http.Request) {
	if viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/", http.StatusFound)
		return
	}

	t := viper.GetTime("config.cert_requested")
	if !t.IsZero() && t.After(time.Now().Add(-5*time.Minute)) {
		// Too soon
		if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
			w.Header().Set("Content-Type", "application/json")
			if viper.GetBool("config.error") {
				viper.Set("config.cert_requested", nil)
				viper.WriteConfig()
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"complete": viper.GetBool("config.complete"), "error": viper.GetBool("config.error")})
		} else {
			render(w, r, "polling:manage", map[string]interface{}{"Progress": _progress("polling"), "HelpText": _helptext("polling")})
		}
		return
	}

	viper.Set("config.cert_requested", time.Now())
	if viper.GetBool("config.error") {
		viper.Set("config.error", false)
	}
	viper.WriteConfig()
	// 9. Setup our own web certificate
	if !_hostCommand(w, r, "acme-request") {
		viper.Set("config.error", true)
		viper.WriteConfig()
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/logs/cert", http.StatusSeeOther)
		return
	}

	// 10. remove the temporary bit from nginx config
	if !_hostCommand(w, r, "nginx-remove-redirect") {
		return
	}

	// 11. reload nginx
	if !_hostCommand(w, r, "nginx-reload") {
		return
	}

	viper.Set("config.complete", true)
	viper.WriteConfig()

	if r.Header.Get("X-Requested-With") == "XMLHttpRequest" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"complete": viper.GetBool("config.complete")})
	} else {
		render(w, r, "final:manage", map[string]interface{}{"RequestBase": r.Header.Get("X-Request-Base"), "Progress": _progress("final"), "HelpText": _helptext("final")})
	}
}

func showErrorHandler(w http.ResponseWriter, r *http.Request) {
	errorHandler(w, r, nil, http.StatusInternalServerError)
}

// RangeStructer takes the first argument, which must be a struct, and
// returns the value of each field in a slice. It will return nil
// if there are no arguments or first argument is not a struct
func RangeStructer(args ...interface{}) []interface{} {
	if len(args) == 0 {
		return nil
	}

	v := reflect.ValueOf(args[0])
	if v.Kind() != reflect.Struct {
		return nil
	}

	out := make([]interface{}, v.NumField())
	for i := 0; i < v.NumField(); i++ {
		switch v.Field(i).Kind() {
		case reflect.String:
			if v.Field(i).Type().String() == "template.HTML" {
				out[i] = template.HTML(v.Field(i).String())
			} else {
				out[i] = v.Field(i).String()
			}
		case reflect.Bool:
			out[i] = v.Field(i).Bool()
		default:
			out[i] = v.Field(i)
		}
	}

	return out
}

func accountsHandler(w http.ResponseWriter, r *http.Request) {
	if !viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	Accounts, err := GetAccounts(w, r)
	if err == nil {
		render(w, r, "list:accounts", map[string]interface{}{"List": Accounts, "Title": "ACME"})
	}
}

func accountHandler(w http.ResponseWriter, r *http.Request) {
	if !viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	vars := mux.Vars(r)
	id := vars["id"]

	AccountDetails, err := GetAccount(w, r, id)
	if err == nil {
		render(w, r, "show:accounts", map[string]interface{}{"Details": AccountDetails, "Title": "ACME"})
	}
}

func ordersHandler(w http.ResponseWriter, r *http.Request) {
	if !viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	Orders, err := GetOrders(w, r, "")
	if err == nil {
		render(w, r, "list:orders", map[string]interface{}{"List": Orders, "Title": "ACME"})
	}
}

func orderHandler(w http.ResponseWriter, r *http.Request) {
	if !viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	vars := mux.Vars(r)
	id := vars["id"]

	OrderDetails, err := GetOrder(w, r, id)
	if err == nil {
		render(w, r, "show:orders", map[string]interface{}{"Details": OrderDetails, "Title": "ACME"})
	}
}

func authzHandler(w http.ResponseWriter, r *http.Request) {
	if !viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	Authz, err := GetAuthzs(w, r, "", []string{})
	if err == nil {
		render(w, r, "list:authz", map[string]interface{}{"List": Authz, "Title": "ACME"})
	}
}

func authHandler(w http.ResponseWriter, r *http.Request) {
	if !viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	vars := mux.Vars(r)
	id := vars["id"]

	AuthDetails, err := GetAuthz(w, r, id)
	if err == nil {
		render(w, r, "show:authz", map[string]interface{}{"Details": AuthDetails, "Title": "ACME"})
	}
}

func challengesHandler(w http.ResponseWriter, r *http.Request) {
	if !viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	Challenges, err := GetChallenges(w, r, "", []string{})
	if err == nil {
		render(w, r, "list:challenges", map[string]interface{}{"List": Challenges, "Title": "ACME"})
	}
}

func challengeHandler(w http.ResponseWriter, r *http.Request) {
	if !viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	vars := mux.Vars(r)
	id := vars["id"]

	ChallengeDetails, err := GetChallenge(w, r, id)
	if err == nil {
		render(w, r, "show:challenges", map[string]interface{}{"Details": ChallengeDetails, "Title": "ACME"})
	}
}

func certificatesHandler(w http.ResponseWriter, r *http.Request) {
	if !viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	Certificates, err := GetCertificates(w, r, "")
	if err == nil {
		render(w, r, "list:certificates", map[string]interface{}{"List": Certificates, "Title": "ACME"})
	}
}

func certificateHandler(w http.ResponseWriter, r *http.Request) {
	if !viper.GetBool("config.complete") {
		http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/setup", http.StatusFound)
		return
	}

	var serial string
	vars := mux.Vars(r)
	id := vars["id"]
	if viper.GetString("backend") != "step-ca" {
		_, err := strconv.Atoi(vars["id"])
		if err != nil {
			serial = vars["id"]
		}
	}

	CertificateDetails, err := GetCertificate(w, r, id, serial)
	if err == nil {
		render(w, r, "show:certificates", map[string]interface{}{"Details": CertificateDetails, "Title": "ACME"})
	}
}

func certRevokeHandler(w http.ResponseWriter, r *http.Request) {
	if !viper.GetBool("config.complete") {
		errorHandler(w, r, errors.New("method not allowed at this point"), http.StatusMethodNotAllowed)
		return
	}

	if r.Method == "POST" {
		if err := r.ParseForm(); err != nil {
			errorHandler(w, r, err, http.StatusInternalServerError)
			return
		}

		serial := r.Form.Get("serial")
		reason, err := strconv.Atoi(r.Form.Get("reason"))
		if err != nil {
			errorHandler(w, r, err, http.StatusBadRequest)
			return
		}

		if !_hostCommand(w, r, "revoke-cert", serial, strconv.Itoa(reason)) {
			return
		}
	}
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	res := parseDockerStats(getLog(w, r, "stats"))

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(res)
}

type navItem struct {
	Name     string
	Icon     string
	Attrs    map[template.HTMLAttr]string
	IsActive bool
	SubMenu  []navItem
}

func _matchPrefix(uri string, prefix string) bool {
	return (uri == prefix || strings.HasPrefix(uri, prefix+"/"))
}

func _acmeNav(active string, uri string, requestBase string) navItem {
	isAcmeActive := _matchPrefix(uri, "/accounts") || _matchPrefix(uri, "/orders") ||
		_matchPrefix(uri, "/authz") || _matchPrefix(uri, "/challenges") ||
		_matchPrefix(uri, "/certificates") || false

	accounts := navItem{
		Name: "Accounts",
		Icon: "fa-list-alt",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/accounts",
			"title": "ACME Accounts",
		},
	}
	orders := navItem{
		Name: "Orders",
		Icon: "fa-tags",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/orders",
			"title": "ACME Orders",
		},
	}
	authz := navItem{
		Name: "Authorizations",
		Icon: "fa-chain",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/authz",
			"title": "ACME Authorizations",
		},
	}
	challenges := navItem{
		Name: "Challenges",
		Icon: "fa-exchange",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/challenges",
			"title": "ACME Challenges",
		},
	}
	certificates := navItem{
		Name: "Certificates",
		Icon: "fa-lock",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/certificates",
			"title": "ACME Certificates",
		},
	}
	acme := navItem{
		Name: "ACME",
		Icon: "fa-sitemap",
		Attrs: map[template.HTMLAttr]string{
			"href":  "#",
			"title": "Automated Certificate Management Environment",
		},
		IsActive: isAcmeActive,
		SubMenu:  []navItem{accounts, certificates, orders, authz, challenges},
	}

	// set active menu class
	switch active {
	case "accounts":
		accounts.Attrs["class"] = "active"
	case "certificates":
		certificates.Attrs["class"] = "active"
	case "orders":
		orders.Attrs["class"] = "active"
	case "authz":
		authz.Attrs["class"] = "active"
	case "challenges":
		challenges.Attrs["class"] = "active"
	}

	return acme
}

func activeNav(active string, uri string, requestBase string) []navItem {
	// create menu items
	home := navItem{
		Name: "Dashboard",
		Icon: "fa-dashboard",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/",
			"title": "Main page with the status of the system",
		},
	}
	acme := _acmeNav(active, uri, requestBase)
	cert := navItem{
		Name: "Web Certificate",
		Icon: "fa-lock",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/logs/cert",
			"title": "Log file for the certificate renewal for this server",
		},
	}
	boulder := navItem{
		Name: "ACME",
		Icon: "fa-search-plus",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/logs/boulder",
			"title": "Live view on the backend ACME application logs",
		},
	}
	audit := navItem{
		Name: "ACME Audit Log",
		Icon: "fa-paw",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/logs/audit",
			"title": "Live view on only the audit messages in the backend ACME application logs",
		},
	}
	labca := navItem{
		Name: "LabCA",
		Icon: "fa-edit",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/logs/labca",
			"title": "Live view on the logs for this LabCA web application",
		},
	}
	cron := navItem{
		Name: "Cron Log",
		Icon: "fa-clock-o",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/logs/cron",
			"title": "Live view on the logs for the cron jobs for LabCA",
		},
	}
	web := navItem{
		Name: "Web Server",
		Icon: "fa-globe",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/logs/web",
			"title": "Live view on the NGINX web server access log",
		},
	}
	logs := navItem{
		Name: "Logs",
		Icon: "fa-files-o",
		Attrs: map[template.HTMLAttr]string{
			"href":  "#",
			"title": "Log Files",
		},
		IsActive: strings.HasPrefix(uri, "/logs/"),
		SubMenu:  []navItem{boulder, audit, cron, labca, cert, web},
	}
	manage := navItem{
		Name: "Manage",
		Icon: "fa-wrench",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/manage",
			"title": "Manage the system",
		},
	}
	about := navItem{
		Name: "About",
		Icon: "fa-comments",
		Attrs: map[template.HTMLAttr]string{
			"href":  requestBase + "/about",
			"title": "About LabCA",
		},
	}
	public := navItem{
		Name: "Public Area",
		Icon: "fa-home",
		Attrs: map[template.HTMLAttr]string{
			"href":  "http://" + viper.GetString("labca.fqdn"),
			"title": "The non-Admin pages of this LabCA instance",
		},
	}

	// set active menu class
	switch active {
	case "about":
		about.Attrs["class"] = "active"
	case "index":
		home.Attrs["class"] = "active"
	case "manage":
		manage.Attrs["class"] = "active"
	}

	if viper.GetBool("standalone") {
		return []navItem{home, acme, manage, about}
	}

	return []navItem{home, acme, logs, manage, about, public}
}

func render(w http.ResponseWriter, r *http.Request, view string, data map[string]interface{}) {
	viewSlice := strings.Split(view, ":")
	menu := viewSlice[0]
	if len(viewSlice) > 1 {
		menu = viewSlice[1]
	}
	data["Menu"] = activeNav(menu, r.RequestURI, r.Header.Get("X-Request-Base"))

	if version != "" {
		data["Version"] = version
	}

	if webTitle != "" {
		data["WebTitle"] = webTitle
	}

	b, err := tmpls.Render("base.tmpl", "views/"+viewSlice[0]+".tmpl", data)
	if err != nil {
		errorHandler(w, r, err, http.StatusInternalServerError)
		return
	}

	w.Write(b)
}

func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	errorHandler(w, r, fmt.Errorf("NotFoundHandler for: %s %s", r.Method, r.URL), http.StatusNotFound)
}

func authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method + " " + r.RequestURI)

		if r.RequestURI == "/login" || strings.Contains(r.RequestURI, "/static/") {
			next.ServeHTTP(w, r)
		} else {
			session, _ := sessionStore.Get(r, "labca")
			if session.Values["user"] != nil || (r.RequestURI == "/setup" && viper.Get("user.password") == nil) {
				// Keep setting the cookie so the expiration / max-age keeps renewing
				if err := session.Save(r, w); err != nil {
					log.Printf("cannot save session: %s\n", err)
				}
				next.ServeHTTP(w, r)
			} else {
				session.Values["bounce"] = r.RequestURI
				if err := session.Save(r, w); err != nil {
					log.Printf("cannot save session: %s\n", err)
				}
				http.Redirect(w, r, r.Header.Get("X-Request-Base")+"/login", http.StatusFound)
			}
		}
	})
}

func init() {
	if os.Getenv("DEVELOPMENT") != "" {
		isDev = true
	}

	address := flag.String("address", "", "Address to listen on (default 0.0.0.0 when using init)")
	configFile := flag.String("config", "", "File path to the configuration file for this application")
	init := flag.Bool("init", false, "Initialize the application for running standalone, create/update the config file")
	port := flag.Int("port", 0, "Port to listen on (default 3000 when using init)")
	versionFlag := flag.Bool("version", false, "Show version number and exit")
	decrypt := flag.String("d", "", "Decrypt a value")
	flag.Parse()

	if *versionFlag {
		fmt.Println(standaloneVersion)
		os.Exit(0)
	}

	if *configFile == "" {
		viper.SetConfigName("config")
		viper.AddConfigPath("data")
		configPath = "./data"
	} else {
		_, err := os.Stat(*configFile)
		if os.IsNotExist(err) {
			viper.WriteConfigAs(*configFile)
		}

		viper.AddConfigPath(filepath.Dir(*configFile))
		configPath = filepath.Dir(*configFile)
		viper.SetConfigName(strings.TrimSuffix(filepath.Base(*configFile), filepath.Ext(*configFile)))
	}
	viper.SetDefault("config.complete", false)
	if err := viper.ReadInConfig(); err != nil {
		panic(fmt.Errorf("fatal error config file: '%s'", err))
	}

	if *decrypt != "" {
		plain, err := _decrypt(*decrypt)
		if err == nil {
			fmt.Println(string(plain))
			os.Exit(0)
		} else {
			os.Exit(1)
		}
	}

	var err error
	if *init || viper.GetBool("standalone") {
		tmpls, err = templates.New().ParseEmbed(embeddedTemplates, "templates/")
	} else {
		tmpls, err = templates.New().ParseDir("./templates", "templates/")
	}
	if err != nil {
		panic(fmt.Errorf("fatal error templates: '%s'", err))
	}

	if viper.Get("keys.auth") == nil {
		key := securecookie.GenerateRandomKey(32)
		if key == nil {
			panic(fmt.Errorf("fatal error random key"))
		}
		viper.Set("keys.auth", base64.StdEncoding.EncodeToString(key))
		viper.WriteConfig()
	}
	if viper.Get("keys.enc") == nil {
		key := securecookie.GenerateRandomKey(32)
		if key == nil {
			panic(fmt.Errorf("fatal error random key"))
		}
		viper.Set("keys.enc", base64.StdEncoding.EncodeToString(key))
		viper.WriteConfig()
	}

	if *init {
		if *address != "" {
			viper.Set("server.addr", *address)
		}
		if *port != 0 {
			viper.Set("server.port", *port)
		}
		viper.Set("standalone", true)
		viper.WriteConfig()
	}

	if viper.Get("server.addr") == nil {
		viper.Set("server.addr", "0.0.0.0")
		viper.WriteConfig()
	}

	if viper.Get("server.port") == nil {
		viper.Set("server.port", 3000)
		viper.WriteConfig()
	}

	if viper.Get("server.session.maxage") == nil {
		viper.Set("server.session.maxage", 3600) // 1 hour
		viper.WriteConfig()
	}

	if viper.Get("db.conn") == nil {
		viper.Set("db.type", "mysql")
		viper.Set("db.conn", "root@tcp(boulder-mysql:3306)/boulder_sa_integration")
		viper.WriteConfig()
	}
	dbConn = viper.GetString("db.conn")
	dbType = viper.GetString("db.type")

	if viper.GetBool("standalone") {
		version = standaloneVersion
	} else {
		version = viper.GetString("version")
		if version == "" {
			version = standaloneVersion
		}
	}

	webTitle = viper.GetString("labca.web_title")
	if webTitle == "" {
		webTitle = "LabCA"
	}

	a := viper.GetString("server.addr")
	p := viper.GetInt("server.port")
	if *address != "" && *address != viper.GetString("server.addr") {
		a = *address
	}
	if *port != 0 && *port != viper.GetInt("server.port") {
		p = *port
	}
	listenAddress = fmt.Sprintf("%s:%d", a, p)

	updateAvailable = false
}

func main() {
	tmpls.Parse()

	keys_auth, err := base64.StdEncoding.DecodeString(viper.GetString("keys.auth"))
	if err != nil {
		log.Fatalf("cannot decode configured 'keys.auth': %s\n", err)
	}
	keys_enc, err := base64.StdEncoding.DecodeString(viper.GetString("keys.enc"))
	if err != nil {
		log.Fatalf("cannot decode configured 'keys.enc': %s\n", err)
	}
	sessionStore = sessions.NewCookieStore(keys_auth, keys_enc)
	sessionStore.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   viper.GetInt("server.session.maxage") * 1,
		HttpOnly: true,
		Secure:   viper.GetBool("server.https"),
	}

	r := mux.NewRouter()
	r.HandleFunc("/", rootHandler).Methods("GET")
	r.HandleFunc("/stats", statsHandler).Methods("GET")
	r.HandleFunc("/about", aboutHandler).Methods("GET")
	r.HandleFunc("/manage", manageHandler).Methods("GET", "POST")
	r.HandleFunc("/final", finalHandler).Methods("GET")
	r.HandleFunc("/error", showErrorHandler).Methods("GET")
	r.HandleFunc("/login", loginHandler).Methods("GET", "POST")
	r.HandleFunc("/logout", logoutHandler).Methods("GET")
	r.HandleFunc("/logs/{type}", logsHandler).Methods("GET")
	r.HandleFunc("/restart", restartHandler).Methods("GET")
	r.HandleFunc("/setup", setupHandler).Methods("GET", "POST")
	r.HandleFunc("/wait", waitHandler).Methods("GET")
	r.HandleFunc("/ws", wsHandler).Methods("GET")

	r.HandleFunc("/accounts", accountsHandler).Methods("GET")
	r.HandleFunc("/accounts/{id}", accountHandler).Methods("GET")
	r.HandleFunc("/orders", ordersHandler).Methods("GET")
	r.HandleFunc("/orders/{id}", orderHandler).Methods("GET")
	r.HandleFunc("/authz", authzHandler).Methods("GET")
	r.HandleFunc("/authz/{id}", authHandler).Methods("GET")
	r.HandleFunc("/challenges", challengesHandler).Methods("GET")
	r.HandleFunc("/challenges/{id}", challengeHandler).Methods("GET")
	r.HandleFunc("/certificates", certificatesHandler).Methods("GET")
	r.HandleFunc("/certificates/{id}", certificateHandler).Methods("GET")
	r.HandleFunc("/certificates/{id}", certRevokeHandler).Methods("POST")

	r.PathPrefix("/backup/").Handler(http.StripPrefix("/backup/", http.FileServer(http.Dir("/opt/backup"))))

	r.NotFoundHandler = http.HandlerFunc(notFoundHandler)
	if viper.GetBool("standalone") || isDev {
		var sfs http.Handler
		if viper.GetBool("standalone") {
			sfs = http.FileServer(http.FS(staticFiles))

			r.PathPrefix("/accounts/static/").Handler(http.StripPrefix("/accounts", sfs))
			r.PathPrefix("/authz/static/").Handler(http.StripPrefix("/authz", sfs))
			r.PathPrefix("/challenges/static/").Handler(http.StripPrefix("/challenges", sfs))
			r.PathPrefix("/certificates/static/").Handler(http.StripPrefix("/certificates", sfs))
			r.PathPrefix("/orders/static/").Handler(http.StripPrefix("/orders", sfs))
			r.PathPrefix("/static/").Handler(sfs)
		}
		if isDev {
			sfs = http.FileServer(http.Dir("static"))

			r.PathPrefix("/accounts/static/").Handler(http.StripPrefix("/accounts/static/", sfs))
			r.PathPrefix("/authz/static/").Handler(http.StripPrefix("/authz/static/", sfs))
			r.PathPrefix("/challenges/static/").Handler(http.StripPrefix("/challenges/static/", sfs))
			r.PathPrefix("/certificates/static/").Handler(http.StripPrefix("/certificates/static/", sfs))
			r.PathPrefix("/orders/static/").Handler(http.StripPrefix("/orders/static/", sfs))
			r.PathPrefix("/logs/static/").Handler(http.StripPrefix("/logs/static/", sfs))
			r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", sfs))
		}
	}
	r.Use(authorized)

	log.Printf("Listening on %s...\n", listenAddress)
	srv = &http.Server{
		Handler:      r,
		Addr:         listenAddress,
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	if viper.GetBool("server.https") {
		log.Fatal(srv.ListenAndServeTLS(viper.GetString("server.cert"), viper.GetString("server.key")))
	} else {
		log.Fatal(srv.ListenAndServe())
	}
}
