package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	_ "expvar"

	"github.com/lib/pq"

	"github.com/gorilla/mux"
	"github.com/spf13/viper"
)

// The following Null* type code was taken from or informed by the blog at
// https://medium.com/aubergine-solutions/how-i-handled-null-possible-values-from-database-rows-in-golang-521fb0ee267.
// and
// https://stackoverflow.com/questions/24564619/nullable-time-time-in-golang

// NullInt64 is an alias of the sql.NullInt64. Having an alias allows us to
// extend the types, which we can't do without the alias because sql.NullInt64
// is defined in another package.
type NullInt64 sql.NullInt64

// Scan implements the Scanner interface for our NullInt64 alias.
func (i *NullInt64) Scan(value interface{}) error {
	var (
		valid bool
		n     sql.NullInt64
		err   error
	)

	if err = n.Scan(value); err != nil {
		return err
	}

	if reflect.TypeOf(value) != nil {
		valid = true
	}

	*i = NullInt64{i.Int64, valid}
	return nil
}

// MarshalJSON implements the json.Marshaler interface for our NullInt64 alias.
func (i *NullInt64) MarshalJSON() ([]byte, error) {
	if !i.Valid {
		return []byte("null"), nil
	}
	return json.Marshal(i.Int64)
}

// NullTime is an alias of pq.NullTime, which allows us to extend the type by
// implementing custom JSON marshalling logic.
type NullTime pq.NullTime

// Scan implements the Scanner interface for the NullTime alias. Basically just
// delegates to the Scan() implementation for pq.NullTime.
func (t *NullTime) Scan(value interface{}) error {
	var (
		valid bool
		nt    pq.NullTime
		err   error
	)

	if err = nt.Scan(value); err != nil {
		return err
	}

	if reflect.TypeOf(value) != nil {
		valid = true
	}

	*t = NullTime{nt.Time, valid}
	return nil
}

// Value implements the Valuer interface for the NullTime alias. Needed for the
// pq driver.
func (t NullTime) Value() (driver.Value, error) {
	if !t.Valid {
		return nil, nil
	}
	return t.Time, nil
}

// MarshalJSON implements the json.Marshaler interface for our NullTime alias.
// We're using this to convert timestamps to int64s containing the milliseconds
// since the epoch.
func (t *NullTime) MarshalJSON() ([]byte, error) {
	if !t.Valid {
		return []byte("null"), nil
	}
	return []byte(strconv.FormatInt(t.Time.UnixNano()/1000000, 10)), nil
}

// Job is an entry from the jobs table in the database. It contains a minimal
// set of fields.
type Job struct {
	ID             string   `json:"id"`
	AppID          string   `json:"app_id"`
	UserID         string   `json:"user_id"`
	Username       string   `json:"username"`
	Status         string   `json:"status"`
	Description    string   `json:"description"`
	Name           string   `json:"name"`
	ResultFolder   string   `json:"result_folder"`
	StartDate      NullTime `json:"start_date"`
	PlannedEndDate NullTime `json:"planned_end_date,omitempty"`
	SystemID       string   `json:"system_id"`
}

// JobList is a list of Jobs. Duh.
type JobList struct {
	Jobs []Job `json:"jobs"`
}

const listJobsQuery = `
SELECT j.id,
       j.app_id,
       j.user_id,
       u.username,
       j.status,
       j.job_description as description,
       j.job_name as name,
       j.result_folder_path as result_folder,
       j.start_date,
       j.planned_end_date,
       t.system_id
  FROM jobs j
  JOIN users u
    ON j.user_id = u.id
  JOIN job_types t
    ON j.job_type_id = t.id
 WHERE j.status = $1
   AND j.planned_end_date <= NOW()`

const jobsToKillInFutureQuery = `
SELECT j.id,
       j.app_id,
       j.user_id,
       u.username,
       j.status,
       j.job_description as description,
       j.job_name as name,
       j.result_folder_path as result_folder,
       j.start_date,
       j.planned_end_date,
       t.system_id
  FROM jobs j
  JOIN users u
    ON j.user_id = u.id
  JOIN job_types t
    ON j.job_type_id = t.id
 WHERE j.status = $1
   AND NOW() < j.planned_end_date
	 AND j.planned_end_date <= NOW() + interval '%d minutes'`

func getJobList(ctx context.Context, db *sql.DB, query, status string) (*JobList, error) {
	rows, err := db.QueryContext(ctx, query, status)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jl := &JobList{
		Jobs: []Job{},
	}

	for rows.Next() {
		var j Job
		err = rows.Scan(
			&j.ID,
			&j.AppID,
			&j.UserID,
			&j.Username,
			&j.Status,
			&j.Description,
			&j.Name,
			&j.ResultFolder,
			&j.StartDate,
			&j.PlannedEndDate,
			&j.SystemID,
		)
		if err != nil {
			return nil, err
		}
		jl.Jobs = append(jl.Jobs, j)
	}
	return jl, nil
}

func listJobs(ctx context.Context, db *sql.DB, status string) (*JobList, error) {
	return getJobList(ctx, db, listJobsQuery, status)
}

func listJobsToKillInFuture(ctx context.Context, db *sql.DB, status string, interval int64) (*JobList, error) {
	return getJobList(ctx, db, fmt.Sprintf(jobsToKillInFutureQuery, interval), status)
}

// StatusUpdate contains the information contained in a status update for an
// analysis in the database
type StatusUpdate struct {
	ID                     string    `json:"id"`          // The analysis ID
	ExternalID             string    `json:"external_id"` // Also referred to as invocation ID
	Status                 string    `json:"status"`
	SentFrom               string    `json:"sent_from"`
	SentOn                 int64     `json:"sent_on"` // Not actually nullable.
	Propagated             bool      `json:"propagated"`
	PropagationAttempts    int64     `json:"propagation_attempts"`
	LastPropagationAttempt NullInt64 `json:"last_propagation_attempt"`
	CreatedDate            NullTime  `json:"created_date"` // Not actually nullable.
}

// StatusUpdates is a list of StatusUpdates. Mostly exists for marshalling a
// list into JSON in a format our other services generally expect.
type StatusUpdates struct {
	Updates []StatusUpdate `json:"status_updates"`
}

const stepTypesQuery = `
SELECT t.name
  FROM jobs j
  JOIN job_steps s
    ON j.id = s.job_id
  JOIN job_types t
    ON s.job_type_id = t.id
 WHERE j.id = $1`

func isInteractive(ctx context.Context, db *sql.DB, id string) (bool, error) {
	rows, err := db.QueryContext(ctx, stepTypesQuery, id)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	var jobTypes []string

	for rows.Next() {
		var t string
		err = rows.Scan(&t)
		if err != nil {
			return false, err
		}
		jobTypes = append(jobTypes, t)
	}

	found := false
	for _, j := range jobTypes {
		if j == "Interactive" {
			found = true
		}
	}

	return found, nil
}

const statusUpdatesByID = `
SELECT j.id,
       u.external_id,
       u.status,
       u.sent_from,
       u.sent_on,
       u.propagated,
       u.propagation_attempts,
       u.last_propagation_attempt,
       u.created_date
  FROM jobs j
  JOIN job_steps s
    ON j.id = s.job_id
  JOIN job_status_updates u
    ON s.external_id = u.external_id
 WHERE j.id = $1
 ORDER BY u.sent_on ASC`

func getStatusUpdates(ctx context.Context, db *sql.DB, id string) (*StatusUpdates, error) {
	rows, err := db.QueryContext(ctx, statusUpdatesByID, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	statusUpdates := &StatusUpdates{
		Updates: []StatusUpdate{},
	}

	for rows.Next() {
		var u StatusUpdate
		err = rows.Scan(
			&u.ID,
			&u.ExternalID,
			&u.Status,
			&u.SentFrom,
			&u.SentOn,
			&u.Propagated,
			&u.PropagationAttempts,
			&u.LastPropagationAttempt,
			&u.CreatedDate,
		)
		if err != nil {
			return nil, err
		}
		statusUpdates.Updates = append(statusUpdates.Updates, u)
	}
	return statusUpdates, nil
}

const getJobByIDQuery = `
SELECT j.id,
       j.app_id,
       j.user_id,
       u.username,
       j.status,
       j.job_description as description,
       j.job_name as name,
       j.result_folder_path as result_folder,
       j.start_date,
       j.planned_end_date,
       t.system_id
  FROM jobs j
  JOIN users u
    ON j.user_id = u.id
  JOIN job_types t
    ON j.job_type_id = t.id
 WHERE j.id = $1`

const getJobByExternalIDQuery = `
 SELECT DISTINCT j.id,
        j.app_id,
        j.user_id,
        users.username,
        j.status,
        j.job_description as description,
        j.job_name as name,
        j.result_folder_path as result_folder,
        j.start_date,
        j.planned_end_date,
        t.system_id
   FROM jobs j
   JOIN job_steps s
     ON j.id = s.job_id
   JOIN job_status_updates u
     ON s.external_id = u.external_id
 	JOIN users
    ON j.user_id = users.id
  JOIN job_types t
    ON j.job_type_id = t.id
  WHERE u.external_id = $1`

func getJob(ctx context.Context, db *sql.DB, query, id string) (*Job, error) {
	var (
		j   Job
		err error
	)

	row := db.QueryRowContext(ctx, query, id)

	if err = row.Scan(
		&j.ID,
		&j.AppID,
		&j.UserID,
		&j.Username,
		&j.Status,
		&j.Description,
		&j.Name,
		&j.ResultFolder,
		&j.StartDate,
		&j.PlannedEndDate,
		&j.SystemID,
	); err != nil {
		return nil, err
	}

	return &j, err
}

func getJobByID(ctx context.Context, db *sql.DB, id string) (*Job, error) {
	return getJob(ctx, db, getJobByIDQuery, id)
}

func getJobByExternalID(ctx context.Context, db *sql.DB, externalID string) (*Job, error) {
	return getJob(ctx, db, getJobByExternalIDQuery, externalID)
}

const updateJobTemplate = `
UPDATE ONLY jobs
  SET %s
WHERE id = $1`

func updateJob(ctx context.Context, db *sql.DB, id string, patch map[string]string) (*Job, error) {
	var err error

	setstring := "%s = '%s'"
	sets := []string{}

	for k, v := range patch {
		sets = append(sets, fmt.Sprintf(setstring, k, v))
	}

	if len(sets) == 0 {
		return nil, errors.New("nothing in patch")
	}

	fullupdate := fmt.Sprintf(updateJobTemplate, strings.Join(sets, ", "))

	if _, err = db.ExecContext(ctx, fullupdate, id); err != nil {
		return nil, err
	}

	j, err := getJobByID(ctx, db, id)
	if err != nil {
		return nil, err
	}

	return j, nil
}

// AnalysesApp contains the application logic.
type AnalysesApp struct {
	db *sql.DB
}

// ExpiredByStatus returns a http.Handler that will respond to requests
// for a list of jobs with the given status that have passed their expiration
// date.
func (a *AnalysesApp) ExpiredByStatus(ctx context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		validStatuses := map[string]bool{
			"Completed": true,
			"Failed":    true,
			"Submitted": true,
			"Queued":    true,
			"Running":   true,
			"Canceled":  true,
		}

		vars := mux.Vars(r)
		status := strings.Title(strings.ToLower(vars["status"]))
		if _, ok := validStatuses[status]; !ok {
			http.Error(w, fmt.Sprintf("unknown status %s", status), http.StatusBadRequest)
			return
		}

		log.Printf("looking up expired analyses with a status of %s\n", status)

		list, err := listJobs(ctx, a.db, status)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set(http.CanonicalHeaderKey("content-type"), "application/json")

		if err = json.NewEncoder(w).Encode(list); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

// ExpiresInByStatus returns an http.Handler that handles requests for lists of
// analyses with the provided status that will expire in the provided number of
// minutes.
func (a *AnalysesApp) ExpiresInByStatus(ctx context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		validStatuses := map[string]bool{
			"Completed": true,
			"Failed":    true,
			"Submitted": true,
			"Queued":    true,
			"Running":   true,
			"Canceled":  true,
		}

		vars := mux.Vars(r)
		status := strings.Title(strings.ToLower(vars["status"]))
		if _, ok := validStatuses[status]; !ok {
			http.Error(w, fmt.Sprintf("unknown status %s", status), http.StatusBadRequest)
			return
		}

		minutes, err := strconv.ParseInt(vars["minutes"], 10, 64)
		if err != nil {
			http.Error(w, fmt.Sprintf("can't parse %s as an integer", vars["minutes"]), http.StatusBadRequest)
			return
		}

		log.Printf("looking up list of jobs with a status of %s that will expire in about %d minutes\n", status, minutes)

		list, err := listJobsToKillInFuture(ctx, a.db, status, minutes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set(http.CanonicalHeaderKey("content-type"), "application/json")

		if err = json.NewEncoder(w).Encode(list); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

// GetByID returns an http.Handler that handles requests for a particular
// analysis with the provided ID.
func (a *AnalysesApp) GetByID(ctx context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		id := vars["id"]

		log.Printf("look up analysis with an ID of %s\n", id)

		job, err := getJobByID(ctx, a.db, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set(http.CanonicalHeaderKey("content-type"), "application/json")

		if err = json.NewEncoder(w).Encode(job); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

// GetByExternalID returns an http.Handler that handles requests for a
// particular analysis that has a step with the given external ID.
func (a *AnalysesApp) GetByExternalID(ctx context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		id := vars["external_id"]

		log.Printf("look up analysis by the external ID %s\n", id)

		job, err := getJobByExternalID(ctx, a.db, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set(http.CanonicalHeaderKey("content-type"), "application/json")

		if err = json.NewEncoder(w).Encode(job); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

// UpdateByID returns an http.Handler that handles requests to update an
// analysis with the provided ID. Only supports updating the status,
// planned_end_date, description, and name fields.
func (a *AnalysesApp) UpdateByID(ctx context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			ok  bool
			err error
		)

		vars := mux.Vars(r)
		id := vars["id"]
		defer r.Body.Close()

		log.Printf("patching analysis with an ID of %s\n", id)

		jobpatch := make(map[string]interface{})

		if err = json.NewDecoder(r.Body).Decode(&jobpatch); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		dbpatch := map[string]string{}

		if _, ok = jobpatch["status"]; ok {
			dbpatch["status"] = jobpatch["status"].(string)
		}

		if _, ok = jobpatch["planned_end_date"]; ok {
			dbpatch["planned_end_date"] = strconv.FormatInt(jobpatch["planned_end_date"].(int64), 10)
		}

		if _, ok = jobpatch["description"]; ok {
			dbpatch["job_description"] = jobpatch["description"].(string)
		}

		if _, ok = jobpatch["name"]; ok {
			dbpatch["job_name"] = jobpatch["name"].(string)
		}

		for k, v := range jobpatch {
			log.Printf("setting %s to %v for analysis %s\n", k, v, id)
		}

		job, err := updateJob(ctx, a.db, id, dbpatch)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err = json.NewEncoder(w).Encode(job); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

// StatusUpdates creates an http.Handler for requests for a list of status
// updates based on the provided analysis ID.
func (a *AnalysesApp) StatusUpdates(ctx context.Context) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		id := vars["id"]

		log.Printf("look up status updates for analysis %s\n", id)

		updates, err := getStatusUpdates(ctx, a.db, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err = json.NewEncoder(w).Encode(updates); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}

func main() {
	var (
		err        error
		listenPort = flag.Int("listen-port", 60000, "The port to listen on.")
		sslCert    = flag.String("ssl-cert", "", "Path to the SSL .crt file.")
		sslKey     = flag.String("ssl-key", "", "Path to the SSL .key file.")
	)

	ctx := context.Background()

	flag.Parse()

	useSSL := false
	if *sslCert != "" || *sslKey != "" {
		if *sslCert == "" {
			log.Fatal("--ssl-cert is required with --ssl-key.")
		}

		if *sslKey == "" {
			log.Fatal("--ssl-key is required with --ssl-cert.")
		}
		useSSL = true
	}

	viper.SetConfigType("yaml")
	viper.SetConfigName("jobservices")
	viper.AddConfigPath("/etc/iplant/de/")
	viper.AddConfigPath("$HOME/.jobservices")
	viper.AddConfigPath(".")
	if err = viper.ReadInConfig(); err != nil {
		log.Fatal(err)
	}

	log.Printf("using config file at %s\n", viper.ConfigFileUsed())

	dbURI := viper.GetString("db.uri")
	dbparsed, err := url.Parse(dbURI)
	if err != nil {
		log.Fatal(err)
	}
	if dbparsed.Scheme == "postgresql" {
		dbparsed.Scheme = "postgres"
	}
	dbURI = dbparsed.String()

	log.Printf("connecting to the %s database on %s:%s\n", dbparsed.Path, dbparsed.Hostname(), dbparsed.Port())
	db, err := sql.Open("postgres", dbURI)
	if err != nil {
		log.Fatal(err)
	}

	if err = db.Ping(); err != nil {
		log.Fatal(err)
	}
	log.Println("connected the database")

	app := &AnalysesApp{
		db: db,
	}

	router := mux.NewRouter()
	router.Handle("/debug/vars", http.DefaultServeMux)
	router.Handle("/expired/{status}", app.ExpiredByStatus(ctx)).Methods("GET")
	router.Handle("/expires-in/{minutes:[0-9]+}/{status}", app.ExpiresInByStatus(ctx)).Methods("GET")

	idPath := "/id/{id:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}}"
	router.Handle(idPath, app.GetByID(ctx)).Methods("GET")
	router.Handle(idPath, app.UpdateByID(ctx)).Methods("PATCH")
	router.Handle(fmt.Sprintf("%s/status-updates", idPath), app.StatusUpdates(ctx)).Methods("GET")

	router.Handle("/external-id/{external_id}", app.GetByExternalID(ctx)).Methods("GET")

	log.Printf("listening for requests on port %d\n", *listenPort)
	addr := fmt.Sprintf(":%d", *listenPort)
	if useSSL {
		log.Fatal(http.ListenAndServeTLS(addr, *sslCert, *sslKey, router))
	} else {
		log.Fatal(http.ListenAndServe(addr, router))
	}

}
