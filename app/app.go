// Copyright 2015 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"encoding/json"
	stderr "errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/tsuru/tsuru/action"
	"github.com/tsuru/tsuru/app/bind"
	"github.com/tsuru/tsuru/auth"
	"github.com/tsuru/tsuru/db"
	"github.com/tsuru/tsuru/errors"
	"github.com/tsuru/tsuru/log"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/quota"
	"github.com/tsuru/tsuru/repository"
	"github.com/tsuru/tsuru/router"
	"github.com/tsuru/tsuru/service"
	"gopkg.in/mgo.v2"
	"gopkg.in/mgo.v2/bson"
)

var Provisioner provision.Provisioner
var AuthScheme auth.Scheme

var (
	nameRegexp  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	cnameRegexp = regexp.MustCompile(`^(\*\.)?[a-zA-Z0-9][\w-.]+$`)

	ErrAlreadyHaveAccess = stderr.New("team already have access to this app")
	ErrNoAccess          = stderr.New("team does not have access to this app")
	ErrCannotOrphanApp   = stderr.New("cannot revoke access from this team, as it's the unique team with access to the app")
	ErrDisabledPlatform  = stderr.New("Disabled Platform, only admin users can create applications with the platform")
)

const (
	// InternalAppName is a reserved name used for token generation. For
	// backward compatibilty and historical purpose, the value remained
	// "tsr" when the name of the daemon changed to "tsurud".
	InternalAppName = "tsr"

	TsuruServicesEnvVar = "TSURU_SERVICES"
	defaultAppDir       = "/home/application/current"
)

// AppLock stores information about a lock hold on the app
type AppLock struct {
	Locked      bool
	Reason      string
	Owner       string
	AcquireDate time.Time
}

func (l *AppLock) String() string {
	if !l.Locked {
		return "Not locked"
	}
	return fmt.Sprintf("App locked by %s, running %s. Acquired in %s",
		l.Owner,
		l.Reason,
		l.AcquireDate.Format(time.RFC3339),
	)
}

// App is the main type in tsuru. An app represents a real world application.
// This struct holds information about the app: its name, address, list of
// teams that have access to it, used platform, etc.
type App struct {
	Env            map[string]bind.EnvVar
	Platform       string `bson:"framework"`
	Name           string
	Ip             string
	CName          []string
	Teams          []string
	TeamOwner      string
	Owner          string
	Deploys        uint
	UpdatePlatform bool
	Lock           AppLock
	Plan           Plan
	Pool           string

	quota.Quota
}

// Units returns the list of units.
func (app *App) Units() ([]provision.Unit, error) {
	return Provisioner.Units(app)
}

// MarshalJSON marshals the app in json format.
func (app *App) MarshalJSON() ([]byte, error) {
	repo, _ := repository.Manager().GetRepository(app.Name)
	result := make(map[string]interface{})
	result["name"] = app.Name
	result["platform"] = app.Platform
	result["teams"] = app.Teams
	units, err := app.Units()
	if err != nil {
		return nil, err
	}
	result["units"] = units
	result["repository"] = repo.ReadWriteURL
	result["ip"] = app.Ip
	result["cname"] = app.CName
	result["owner"] = app.Owner
	result["pool"] = app.Pool
	result["deploys"] = app.Deploys
	result["teamowner"] = app.TeamOwner
	result["plan"] = app.Plan
	result["lock"] = app.Lock
	return json.Marshal(&result)
}

// Applog represents a log entry.
type Applog struct {
	Date    time.Time
	Message string
	Source  string
	AppName string
	Unit    string
}

// AcquireApplicationLock acquires an application lock by setting the lock
// field in the database.  This method is already called by a connection
// middleware on requests with :app or :appname params that have side-effects.
func AcquireApplicationLock(appName string, owner string, reason string) (bool, error) {
	return AcquireApplicationLockWait(appName, owner, reason, 0)
}

// Same as AcquireApplicationLock but it keeps trying to acquire the lock
// until timeout is reached.
func AcquireApplicationLockWait(appName string, owner string, reason string, timeout time.Duration) (bool, error) {
	timeoutChan := time.After(timeout)
	conn, err := db.Conn()
	if err != nil {
		return false, err
	}
	defer conn.Close()
	for {
		appLock := AppLock{
			Locked:      true,
			Reason:      reason,
			Owner:       owner,
			AcquireDate: time.Now().In(time.UTC),
		}
		err = conn.Apps().Update(bson.M{"name": appName, "lock.locked": bson.M{"$in": []interface{}{false, nil}}}, bson.M{"$set": bson.M{"lock": appLock}})
		if err == nil {
			return true, nil
		}
		if err != mgo.ErrNotFound {
			return false, err
		}
		select {
		case <-timeoutChan:
			return false, nil
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// ReleaseApplicationLock releases a lock hold on an app, currently it's called
// by a middleware, however, ideally, it should be called individually by each
// handler since they might be doing operations in background.
func ReleaseApplicationLock(appName string) {
	conn, err := db.Conn()
	if err != nil {
		log.Errorf("Error getting DB, couldn't unlock %s: %s", appName, err.Error())
	}
	defer conn.Close()
	err = conn.Apps().Update(bson.M{"name": appName, "lock.locked": true}, bson.M{"$set": bson.M{"lock": AppLock{}}})
	if err != nil {
		log.Errorf("Error updating entry, couldn't unlock %s: %s", appName, err.Error())
	}
}

// GetByName queries the database to find an app identified by the given
// name.
func GetByName(name string) (*App, error) {
	var app App
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	err = conn.Apps().Find(bson.M{"name": name}).One(&app)
	if err == mgo.ErrNotFound {
		return nil, ErrAppNotFound
	}
	return &app, err
}

// CreateApp creates a new app.
//
// Creating a new app is a process composed of the following steps:
//
//       1. Save the app in the database
//       2. Create the git repository using the repository manager
//       3. Provision the app using the provisioner
func CreateApp(app *App, user *auth.User) error {
	teams, err := user.Teams()
	if err != nil {
		return err
	}
	if len(teams) == 0 {
		return NoTeamsError{}
	}
	platform, err := getPlatform(app.Platform)
	if err != nil {
		return err
	}
	if platform.Disabled && !user.IsAdmin() {
		return InvalidPlatformError{}
	}
	var plan *Plan
	if app.Plan.Name == "" {
		plan, err = DefaultPlan()
	} else {
		plan, err = findPlanByName(app.Plan.Name)
	}
	if err != nil {
		return err
	}
	if app.TeamOwner == "" {
		if len(teams) > 1 {
			return ManyTeamsError{}
		}
		app.TeamOwner = teams[0].Name
	}
	err = app.ValidateTeamOwner(user)
	if err != nil {
		return err
	}
	app.Plan = *plan
	err = app.SetPool()
	if err != nil {
		return err
	}
	app.Teams = []string{app.TeamOwner}
	app.Owner = user.Email
	err = app.validate()
	if err != nil {
		return err
	}
	actions := []*action.Action{
		&reserveUserApp,
		&insertApp,
		&exportEnvironmentsAction,
		&createRepository,
		&provisionApp,
		&setAppIp,
	}
	pipeline := action.NewPipeline(actions...)
	err = pipeline.Execute(app, user)
	if err != nil {
		return &AppCreationError{app: app.Name, Err: err}
	}
	return nil
}

// ChangePlan changes the plan of the application.
//
// It may change the state of the application if the new plan includes a new
// router or a change in the amount of available memory.
func (app *App) ChangePlan(planName string, w io.Writer) error {
	plan, err := findPlanByName(planName)
	if err != nil {
		return err
	}
	var oldPlan Plan
	oldPlan, app.Plan = app.Plan, *plan
	actions := []*action.Action{
		&moveRouterUnits,
		&saveApp,
		&restartApp,
		&removeOldBackend,
	}
	return action.NewPipeline(actions...).Execute(app, &oldPlan, w)
}

// unbind takes all service instances that are bound to the app, and unbind
// them. This method is used by Destroy (before destroying the app, it unbinds
// all service instances). Refer to Destroy docs for more details.
func (app *App) unbind() error {
	instances, err := app.serviceInstances()
	if err != nil {
		return err
	}
	var msg string
	var addMsg = func(instanceName string, reason error) {
		if msg == "" {
			msg = "Failed to unbind the following instances:\n"
		}
		msg += fmt.Sprintf("- %s (%s)", instanceName, reason.Error())
	}
	for _, instance := range instances {
		err = instance.UnbindApp(app, nil)
		if err != nil {
			addMsg(instance.Name, err)
		}
	}
	if msg != "" {
		return stderr.New(msg)
	}
	return nil
}

// Delete deletes an app.
func Delete(app *App, w io.Writer) error {
	isSwapped, swappedWith, err := router.IsSwapped(app.GetName())
	if err != nil {
		return fmt.Errorf("unable to check if app is swapped: %s", err)
	}
	if isSwapped {
		return fmt.Errorf("application is swapped with %q, cannot remove it", swappedWith)
	}
	appName := app.Name
	if w == nil {
		w = ioutil.Discard
	}
	fmt.Fprintf(w, "---- Removing application %q...\n", appName)
	var hasErrors bool
	defer func() {
		var problems string
		if hasErrors {
			problems = " Some errors occurred during removal."
		}
		fmt.Fprintf(w, "---- Done removing application.%s\n", problems)
	}()
	logErr := func(msg string, err error) {
		msg = fmt.Sprintf("%s: %s", msg, err)
		fmt.Fprintf(w, "%s\n", msg)
		log.Errorf("[delete-app: %s] %s", appName, msg)
		hasErrors = true
	}
	err = Provisioner.Destroy(app)
	if err != nil {
		logErr("Unable to destroy app in provisioner", err)
	}
	err = app.unbind()
	if err != nil {
		logErr("Unable to unbind app", err)
	}
	err = repository.Manager().RemoveRepository(appName)
	if err != nil {
		logErr("Unable to remove app from repository manager", err)
	}
	token := app.Env["TSURU_APP_TOKEN"].Value
	err = AuthScheme.AppLogout(token)
	if err != nil {
		logErr("Unable to remove app token in destroy", err)
	}
	owner, err := auth.GetUserByEmail(app.Owner)
	if err == nil {
		err = auth.ReleaseApp(owner)
	}
	if err != nil {
		logErr("Unable to release app quota", err)
	}
	logConn, err := db.LogConn()
	if err == nil {
		defer logConn.Close()
		err = logConn.Logs(appName).DropCollection()
	}
	if err != nil {
		logErr("Unable to remove logs collection", err)
	}
	conn, err := db.Conn()
	if err == nil {
		defer conn.Close()
		err = conn.Apps().Remove(bson.M{"name": appName})
	}
	if err != nil {
		logErr("Unable to remove app from db", err)
	}
	err = markDeploysAsRemoved(appName)
	if err != nil {
		logErr("Unable to mark old deploys as removed", err)
	}
	return nil
}

func (app *App) BindUnit(unit *provision.Unit) error {
	instances, err := app.serviceInstances()
	if err != nil {
		return err
	}
	for _, instance := range instances {
		err = instance.BindUnit(app, unit)
		if err != nil {
			log.Errorf("Error binding the unit %s with the service instance %s: %s", unit.ID, instance.Name, err)
		}
	}
	return nil
}

func (app *App) UnbindUnit(unit *provision.Unit) error {
	instances, err := app.serviceInstances()
	if err != nil {
		return err
	}
	for _, instance := range instances {
		err = instance.UnbindUnit(app, unit)
		if err != nil {
			log.Errorf("Error unbinding the unit %s with the service instance %s: %s", unit.ID, instance.Name, err)
		}
	}
	return nil
}

func (app *App) serviceInstances() ([]service.ServiceInstance, error) {
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var instances []service.ServiceInstance
	q := bson.M{"apps": bson.M{"$in": []string{app.Name}}}
	err = conn.ServiceInstances().Find(q).All(&instances)
	if err != nil {
		return nil, err
	}
	return instances, nil
}

// AddUnits creates n new units within the provisioner, saves new units in the
// database and enqueues the apprc serialization.
func (app *App) AddUnits(n uint, process string, writer io.Writer) error {
	if n == 0 {
		return stderr.New("Cannot add zero units.")
	}
	err := action.NewPipeline(
		&reserveUnitsToAdd,
		&provisionAddUnits,
	).Execute(app, n, writer, process)
	return err
}

// RemoveUnits removes n units from the app. It's a process composed of
// multiple steps:
//
//     1. Remove units from the provisioner
//     2. Update quota
func (app *App) RemoveUnits(n uint, process string, writer io.Writer) error {
	err := Provisioner.RemoveUnits(app, n, process, writer)
	if err != nil {
		return err
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	units, err := app.Units()
	if err != nil {
		return err
	}
	return conn.Apps().Update(
		bson.M{"name": app.Name},
		bson.M{
			"$set": bson.M{
				"quota.inuse": len(units),
			},
		},
	)
}

// SetUnitStatus changes the status of the given unit.
func (app *App) SetUnitStatus(unitName string, status provision.Status) error {
	units, err := app.Units()
	if err != nil {
		return err
	}
	for _, unit := range units {
		if strings.HasPrefix(unit.ID, unitName) {
			return Provisioner.SetUnitStatus(unit, status)
		}
	}
	return provision.ErrUnitNotFound
}

type UpdateUnitsData struct {
	ID     string
	Name   string
	Status provision.Status
}

type UpdateUnitsResult struct {
	ID    string
	Found bool
}

// UpdateUnitsStatus updates the status of the given units, returning a map
// which units were found during the update.
func UpdateUnitsStatus(units []UpdateUnitsData) ([]UpdateUnitsResult, error) {
	result := make([]UpdateUnitsResult, len(units))
	for i, unitData := range units {
		unit := provision.Unit{ID: unitData.ID, Name: unitData.Name}
		err := Provisioner.SetUnitStatus(unit, unitData.Status)
		result[i] = UpdateUnitsResult{
			ID:    unitData.ID,
			Found: err != provision.ErrUnitNotFound,
		}
		if err != nil && err != provision.ErrUnitNotFound {
			return nil, err
		}
	}
	return result, nil
}

// Available returns true if at least one of N units is started or unreachable.
func (app *App) Available() bool {
	units, err := app.Units()
	if err != nil {
		return false
	}
	for _, unit := range units {
		if unit.Available() {
			return true
		}
	}
	return false
}

func (app *App) findTeam(team *auth.Team) (int, bool) {
	for i, teamName := range app.Teams {
		if teamName == team.Name {
			return i, true
		}
	}
	return -1, false
}

// Grant allows a team to have access to an app. It returns an error if the
// team already have access to the app.
func (app *App) Grant(team *auth.Team) error {
	if _, found := app.findTeam(team); found {
		return ErrAlreadyHaveAccess
	}
	app.Teams = append(app.Teams, team.Name)
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Apps().Update(bson.M{"name": app.Name}, bson.M{"$addToSet": bson.M{"teams": team.Name}})
	if err != nil {
		return err
	}
	for _, user := range team.Users {
		err = repository.Manager().GrantAccess(app.Name, user)
		if err != nil {
			conn.Apps().Update(bson.M{"name": app.Name}, bson.M{"$pull": bson.M{"teams": team.Name}})
			return err
		}
	}
	return nil
}

// Revoke removes the access from a team. It returns an error if the team do
// not have access to the app.
func (app *App) Revoke(team *auth.Team) error {
	if len(app.Teams) == 1 {
		return ErrCannotOrphanApp
	}
	index, found := app.findTeam(team)
	if !found {
		return ErrNoAccess
	}
	last := len(app.Teams) - 1
	app.Teams[index] = app.Teams[last]
	app.Teams = app.Teams[:last]
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Apps().Update(bson.M{"name": app.Name}, bson.M{"$pull": bson.M{"teams": team.Name}})
	if err != nil {
		return err
	}
	for _, user := range app.usersToRevoke(team) {
		err = repository.Manager().RevokeAccess(app.Name, user)
		if err != nil {
			conn.Apps().Update(bson.M{"name": app.Name}, bson.M{"$addToSet": bson.M{"teams": team.Name}})
			return err
		}
	}
	return nil
}

func (app *App) usersToRevoke(t *auth.Team) []string {
	teams := app.GetTeams()
	users := make([]string, 0, len(t.Users))
	for _, email := range t.Users {
		found := false
		user := auth.User{Email: email}
		for _, team := range teams {
			if team.ContainsUser(&user) {
				found = true
				break
			}
		}
		if !found {
			users = append(users, email)
		}
	}
	return users
}

// GetTeams returns a slice of teams that have access to the app.
func (app *App) GetTeams() []auth.Team {
	var teams []auth.Team
	conn, err := db.Conn()
	if err != nil {
		log.Errorf("Failed to connect to the database: %s", err)
		return nil
	}
	defer conn.Close()
	conn.Teams().Find(bson.M{"_id": bson.M{"$in": app.Teams}}).All(&teams)
	return teams
}

// SetTeamOwner sets the TeamOwner value.
func (app *App) SetTeamOwner(team *auth.Team, u *auth.User) error {
	app.TeamOwner = team.Name
	err := app.ValidateTeamOwner(u)
	if err != nil {
		return err
	}
	app.Grant(team)
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Apps().Update(bson.M{"name": app.Name}, app)
	if err != nil {
		return err
	}
	return nil
}

func (app *App) ValidateTeamOwner(user *auth.User) error {
	if _, err := auth.GetTeam(app.TeamOwner); err == auth.ErrTeamNotFound {
		return err
	}
	if user.IsAdmin() {
		return nil
	}
	teams, err := user.Teams()
	if err != nil {
		return err
	}
	for _, t := range teams {
		if t.Name == app.TeamOwner {
			return nil
		}
	}
	errorMsg := fmt.Sprintf("You can not set %s team as app's owner. Please set one of your teams as app's owner.", app.TeamOwner)
	return stderr.New(errorMsg)
}

func (app *App) SetPool() error {
	pool, err := app.GetPoolForApp(app.Pool)
	if err != nil {
		return err
	}
	if pool == "" {
		pool, err = app.GetDefaultPool()
		if err != nil {
			return err
		}
	}
	app.Pool = pool
	return nil
}

func (app *App) GetPoolForApp(poolName string) (string, error) {
	var query bson.M
	var poolTeam bool
	if poolName != "" {
		query = bson.M{"_id": poolName}
	} else {
		query = bson.M{"teams": app.TeamOwner}
	}
	pools, err := provision.ListPools(query)
	if err != nil {
		return "", err
	}
	if len(pools) > 1 {
		return "", stderr.New("you have access to more than one pool, please choose one in app creation")
	}
	if len(pools) == 0 {
		if poolName == "" {
			return "", nil
		}
		return "", stderr.New("pool not found")
	}
	for _, team := range pools[0].Teams {
		if team == app.TeamOwner {
			poolTeam = true
			break
		}
	}
	if !pools[0].Public && !poolTeam {
		return "", stderr.New(fmt.Sprintf("You don't have access to pool %s", poolName))
	}
	return pools[0].Name, nil
}

func (app *App) GetDefaultPool() (string, error) {
	pools, err := provision.ListPools(bson.M{"default": true})
	if err != nil {
		return "", err
	}
	if len(pools) == 0 {
		return "", stderr.New("No default pool.")
	}
	return pools[0].Name, nil
}

func (app *App) ChangePool(newPoolName string) error {
	poolName, err := app.GetPoolForApp(newPoolName)
	if err != nil {
		return err
	}
	if poolName == "" {
		return stderr.New("This pool doesn't exists.")
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Apps().Update(
		bson.M{"name": app.Name},
		bson.M{"$set": bson.M{"pool": poolName}},
	)
	if err != nil {
		return err
	}
	return nil
}

// setEnv sets the given environment variable in the app.
func (app *App) setEnv(env bind.EnvVar) {
	if app.Env == nil {
		app.Env = make(map[string]bind.EnvVar)
	}
	app.Env[env.Name] = env
	if env.Public {
		app.Log(fmt.Sprintf("setting env %s with value %s", env.Name, env.Value), "tsuru", "api")
	}
}

// getEnv returns the environment variable if it's declared in the app. It will
// return an error if the variable is not defined in this app.
func (app *App) getEnv(name string) (bind.EnvVar, error) {
	var (
		env bind.EnvVar
		err error
		ok  bool
	)
	if env, ok = app.Env[name]; !ok {
		err = stderr.New("Environment variable not declared for this app.")
	}
	return env, err
}

// validate checks app name format
func (app *App) validate() error {
	if app.Name == InternalAppName || !nameRegexp.MatchString(app.Name) {
		msg := "Invalid app name, your app should have at most 63 " +
			"characters, containing only lower case letters, numbers or dashes, " +
			"starting with a letter."
		return &errors.ValidationError{Message: msg}
	}
	return nil
}

// InstanceEnv returns a map of environment variables that belongs to the given
// service instance (identified by the name only).
func (app *App) InstanceEnv(name string) map[string]bind.EnvVar {
	envs := make(map[string]bind.EnvVar)
	for k, env := range app.Env {
		if env.InstanceName == name {
			envs[k] = bind.EnvVar(env)
		}
	}
	return envs
}

// Run executes the command in app units, sourcing apprc before running the
// command.
func (app *App) Run(cmd string, w io.Writer, once bool, interactive bool) error {
	if !app.Available() {
		return stderr.New("App must be available to run commands")
	}
	app.Log(fmt.Sprintf("running '%s'", cmd), "tsuru", "api")
	logWriter := LogWriter{App: app, Source: "app-run"}
	logWriter.Async()
	defer logWriter.Close()
	return app.sourced(cmd, io.MultiWriter(w, &logWriter), once, interactive)
}

func (app *App) sourced(cmd string, w io.Writer, once bool, interactive bool) error {
	source := "[ -f /home/application/apprc ] && source /home/application/apprc"
	cd := fmt.Sprintf("[ -d %s ] && cd %s", defaultAppDir, defaultAppDir)
	cmd = fmt.Sprintf("%s; %s; %s", source, cd, cmd)
	return app.run(cmd, w, once, interactive)
}

func (app *App) run(cmd string, w io.Writer, once bool, interactive bool) error {
	if once {
		if interactive {
			return Provisioner.ExecuteCommandOnce(w, w, app, cmd, "-i", "-t")
		} else {
			return Provisioner.ExecuteCommandOnce(w, w, app, cmd)
		}
	}
	return Provisioner.ExecuteCommand(w, w, app, cmd)
}

// Restart runs the restart hook for the app, writing its output to w.
func (app *App) Restart(process string, w io.Writer) error {
	msg := fmt.Sprintf("---- Restarting process %q ----\n", process)
	if process == "" {
		msg = fmt.Sprintf("---- Restarting the app %q ----\n", app.Name)
	}
	err := log.Write(w, []byte(msg))
	if err != nil {
		log.Errorf("[restart] error on write app log for the app %s - %s", app.Name, err)
		return err
	}
	err = Provisioner.Restart(app, process, w)
	if err != nil {
		log.Errorf("[restart] error on restart the app %s - %s", app.Name, err)
		return err
	}
	return nil
}

func (app *App) Stop(w io.Writer, process string) error {
	msg := fmt.Sprintf("\n ---> Stopping the process %q\n", process)
	if process == "" {
		msg = fmt.Sprintf("\n ---> Stopping the app %q\n", app.Name)
	}
	log.Write(w, []byte(msg))
	err := Provisioner.Stop(app, process)
	if err != nil {
		log.Errorf("[stop] error on stop the app %s - %s", app.Name, err)
		return err
	}
	return nil
}

// GetUnits returns the internal list of units converted to bind.Unit.
func (app *App) GetUnits() ([]bind.Unit, error) {
	var units []bind.Unit
	provUnits, err := app.Units()
	if err != nil {
		return nil, err
	}
	for _, unit := range provUnits {
		units = append(units, &unit)
	}
	return units, nil
}

// GetName returns the name of the app.
func (app *App) GetName() string {
	return app.Name
}

// GetPool returns the pool of the app.
func (app *App) GetPool() string {
	return app.Pool
}

// GetTeamOwner returns the team owner of the app.
func (app *App) GetTeamOwner() string {
	return app.TeamOwner
}

// GetTeamsNames returns the names of teams app.
func (app *App) GetTeamsName() []string {
	return app.Teams
}

// GetMemory returns the memory limit (in bytes) for the app.
func (app *App) GetMemory() int64 {
	return app.Plan.Memory
}

// GetSwap returns the swap limit (in bytes) for the app.
func (app *App) GetSwap() int64 {
	return app.Plan.Swap
}

// GetCpuShare returns the cpu share for the app.
func (app *App) GetCpuShare() int {
	return app.Plan.CpuShare
}

// GetIp returns the ip of the app.
func (app *App) GetIp() string {
	return app.Ip
}

func (app *App) GetQuota() quota.Quota {
	return app.Quota
}

func (app *App) SetQuotaInUse(inUse int) error {
	if app.Quota.Unlimited() {
		return stderr.New("cannot set quota usage for unlimited quota")
	}
	if inUse < 0 {
		return stderr.New("invalid value, cannot be lesser than 0")
	}
	if inUse > app.Quota.Limit {
		return &quota.QuotaExceededError{
			Requested: uint(inUse),
			Available: uint(app.Quota.Limit),
		}
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Apps().Update(bson.M{"name": app.Name}, bson.M{"$set": bson.M{"quota.inuse": inUse}})
	if err == mgo.ErrNotFound {
		return ErrAppNotFound
	}
	return err
}

// GetPlatform returns the platform of the app.
func (app *App) GetPlatform() string {
	return app.Platform
}

// GetDeploys returns the amount of deploys of an app.
func (app *App) GetDeploys() uint {
	return app.Deploys
}

// Envs returns a map representing the apps environment variables.
func (app *App) Envs() map[string]bind.EnvVar {
	return app.Env
}

// SetEnvs saves a list of environment variables in the app. The publicOnly
// parameter indicates whether only public variables can be overridden (if set
// to false, SetEnvs may override a private variable).
func (app *App) SetEnvs(envs []bind.EnvVar, publicOnly bool, w io.Writer) error {
	units, err := app.GetUnits()
	if err != nil {
		return err
	}
	if len(units) > 0 {
		return app.setEnvsToApp(envs, publicOnly, true, w)
	}
	return app.setEnvsToApp(envs, publicOnly, false, w)
}

// setEnvsToApp adds environment variables to an app, serializing the resulting
// list of environment variables in all units of apps. This method can
// serialize them directly or using a queue.
//
// Besides the slice of environment variables, this method also takes two other
// parameters: publicOnly indicates whether only public variables can be
// overridden (if set to false, setEnvsToApp may override a private variable).
//
// shouldRestart defines if the server should be restarted after saving vars.
func (app *App) setEnvsToApp(envs []bind.EnvVar, publicOnly, shouldRestart bool, w io.Writer) error {
	if len(envs) == 0 {
		return nil
	}
	if w != nil {
		fmt.Fprintf(w, "---- Setting %d new environment variables ----\n", len(envs))
	}
	for _, env := range envs {
		set := true
		if publicOnly {
			e, err := app.getEnv(env.Name)
			if err == nil && !e.Public && e.InstanceName != "" {
				set = false
			}
		}
		if set {
			app.setEnv(env)
		}
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Apps().Update(bson.M{"name": app.Name}, bson.M{"$set": bson.M{"env": app.Env}})
	if err != nil {
		return err
	}
	if !shouldRestart {
		return nil
	}
	return Provisioner.Restart(app, "", w)
}

// UnsetEnvs removes environment variables from an app, serializing the
// remaining list of environment variables to all units of the app.
//
// Besides the slice with the name of the variables, this method also takes the
// parameter publicOnly, which indicates whether only public variables can be
// overridden (if set to false, setEnvsToApp may override a private variable).
func (app *App) UnsetEnvs(variableNames []string, publicOnly bool, w io.Writer) error {
	units, err := app.GetUnits()
	if err != nil {
		return err
	}
	if len(units) > 0 {
		return app.unsetEnvsToApp(variableNames, publicOnly, true, w)
	}
	return app.unsetEnvsToApp(variableNames, publicOnly, false, w)
}

func (app *App) unsetEnvsToApp(variableNames []string, publicOnly, shouldRestart bool, w io.Writer) error {
	if len(variableNames) == 0 {
		return nil
	}
	if w != nil {
		fmt.Fprintf(w, "---- Unsetting %d environment variables ----\n", len(variableNames))
	}
	for _, name := range variableNames {
		var unset bool
		e, err := app.getEnv(name)
		if !publicOnly || (err == nil && e.Public) {
			unset = true
		}
		if unset {
			delete(app.Env, name)
		}
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	err = conn.Apps().Update(bson.M{"name": app.Name}, bson.M{"$set": bson.M{"env": app.Env}})
	if err != nil {
		return err
	}
	if !shouldRestart {
		return nil
	}
	return Provisioner.Restart(app, "", w)
}

// AddCName adds a CName to app. It updates the attribute,
// calls the SetCName function on the provisioner and saves
// the app in the database, returning an error when it cannot save the change
// in the database or add the CName on the provisioner.
func (app *App) AddCName(cnames ...string) error {
	for _, cname := range cnames {
		if cname != "" && !cnameRegexp.MatchString(cname) {
			return stderr.New("Invalid cname")
		}
		if cnameExists(cname) {
			return stderr.New("cname already exists!")
		}
		if s, ok := Provisioner.(provision.CNameManager); ok {
			if err := s.SetCName(app, cname); err != nil {
				return err
			}
		}
		conn, err := db.Conn()
		if err != nil {
			return err
		}
		defer conn.Close()
		app.CName = append(app.CName, cname)
		err = conn.Apps().Update(
			bson.M{"name": app.Name},
			bson.M{"$push": bson.M{"cname": cname}},
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func (app *App) RemoveCName(cnames ...string) error {
	for _, cname := range cnames {
		count := 0
		for _, appCname := range app.CName {
			if cname == appCname {
				count += 1
			}
		}
		if count == 0 {
			return stderr.New("cname not exists!")
		}
		if s, ok := Provisioner.(provision.CNameManager); ok {
			if err := s.UnsetCName(app, cname); err != nil {
				return err
			}
		}
		conn, err := db.Conn()
		if err != nil {
			return err
		}
		defer conn.Close()
		err = conn.Apps().Update(
			bson.M{"name": app.Name},
			bson.M{"$pull": bson.M{"cname": cname}},
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func cnameExists(cname string) bool {
	conn, _ := db.Conn()
	defer conn.Close()
	cnames, _ := conn.Apps().Find(bson.M{"cname": cname}).Count()
	if cnames > 0 {
		return true
	}
	return false
}

func (app *App) parsedTsuruServices() map[string][]bind.ServiceInstance {
	var tsuruServices map[string][]bind.ServiceInstance
	if servicesEnv, ok := app.Env[TsuruServicesEnvVar]; ok {
		json.Unmarshal([]byte(servicesEnv.Value), &tsuruServices)
	} else {
		tsuruServices = make(map[string][]bind.ServiceInstance)
	}
	return tsuruServices
}

func (app *App) AddInstance(serviceName string, instance bind.ServiceInstance, writer io.Writer) error {
	tsuruServices := app.parsedTsuruServices()
	serviceInstances := tsuruServices[serviceName]
	serviceInstances = append(serviceInstances, instance)
	tsuruServices[serviceName] = serviceInstances
	servicesJson, err := json.Marshal(tsuruServices)
	if err != nil {
		return err
	}
	if len(instance.Envs) == 0 {
		return nil
	}
	envVars := make([]bind.EnvVar, 0, len(instance.Envs)+1)
	for k, v := range instance.Envs {
		envVars = append(envVars, bind.EnvVar{
			Name:         k,
			Value:        v,
			Public:       false,
			InstanceName: instance.Name,
		})
	}
	envVars = append(envVars, bind.EnvVar{
		Name:   TsuruServicesEnvVar,
		Value:  string(servicesJson),
		Public: false,
	})
	return app.SetEnvs(envVars, false, writer)
}

func findServiceEnv(tsuruServices map[string][]bind.ServiceInstance, name string) (string, string) {
	for _, serviceInstances := range tsuruServices {
		for _, instance := range serviceInstances {
			if instance.Envs[name] != "" {
				return instance.Name, instance.Envs[name]
			}
		}
	}
	return "", ""
}

func (app *App) RemoveInstance(serviceName string, instance bind.ServiceInstance, writer io.Writer) error {
	tsuruServices := app.parsedTsuruServices()
	toUnsetEnvs := make([]string, 0, len(instance.Envs))
	for varName := range instance.Envs {
		toUnsetEnvs = append(toUnsetEnvs, varName)
	}
	index := -1
	serviceInstances := tsuruServices[serviceName]
	for i, si := range serviceInstances {
		if si.Name == instance.Name {
			index = i
			break
		}
	}
	var servicesJson []byte
	var err error
	if index >= 0 {
		for i := index; i < len(serviceInstances)-1; i++ {
			serviceInstances[i] = serviceInstances[i+1]
		}
		tsuruServices[serviceName] = serviceInstances[:len(serviceInstances)-1]
		servicesJson, err = json.Marshal(tsuruServices)
		if err != nil {
			return err
		}
	}
	var envsToSet []bind.EnvVar
	for _, varName := range toUnsetEnvs {
		instanceName, envValue := findServiceEnv(tsuruServices, varName)
		if envValue == "" || instanceName == "" {
			break
		}
		envsToSet = append(envsToSet, bind.EnvVar{
			Name:         varName,
			Value:        envValue,
			Public:       false,
			InstanceName: instanceName,
		})
	}
	if servicesJson != nil {
		envsToSet = append(envsToSet, bind.EnvVar{
			Name:   TsuruServicesEnvVar,
			Value:  string(servicesJson),
			Public: false,
		})
	}
	if len(toUnsetEnvs) > 0 {
		units, err := app.GetUnits()
		if err != nil {
			return err
		}
		shouldRestart := len(envsToSet) == 0 && len(units) > 0
		err = app.unsetEnvsToApp(toUnsetEnvs, false, shouldRestart, writer)
		if err != nil {
			return err
		}
	}
	return app.SetEnvs(envsToSet, false, writer)
}

// Log adds a log message to the app. Specifying a good source is good so the
// user can filter where the message come from.
func (app *App) Log(message, source, unit string) error {
	messages := strings.Split(message, "\n")
	logs := make([]interface{}, 0, len(messages))
	for _, msg := range messages {
		if msg != "" {
			l := Applog{
				Date:    time.Now().In(time.UTC),
				Message: msg,
				Source:  source,
				AppName: app.Name,
				Unit:    unit,
			}
			logs = append(logs, l)
		}
	}
	if len(logs) > 0 {
		notify(app.Name, logs)
		conn, err := db.LogConn()
		if err != nil {
			return err
		}
		defer conn.Close()
		return conn.Logs(app.Name).Insert(logs...)
	}
	return nil
}

// LastLogs returns a list of the last `lines` log of the app, matching the
// fields in the log instance received as an example.
func (app *App) LastLogs(lines int, filterLog Applog) ([]Applog, error) {
	conn, err := db.LogConn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	logs := []Applog{}
	q := bson.M{}
	if filterLog.Source != "" {
		q["source"] = filterLog.Source
	}
	if filterLog.Unit != "" {
		q["unit"] = filterLog.Unit
	}
	err = conn.Logs(app.Name).Find(q).Sort("-$natural").Limit(lines).All(&logs)
	if err != nil {
		return nil, err
	}
	l := len(logs)
	for i := 0; i < l/2; i++ {
		logs[i], logs[l-1-i] = logs[l-1-i], logs[i]
	}
	return logs, nil
}

type Filter struct {
	Name      string
	Platform  string
	TeamOwner string
	UserOwner string
	Locked    bool
}

func (f *Filter) Query() bson.M {
	query := bson.M{}
	if f == nil {
		return query
	}
	if f.Name != "" {
		query["name"] = bson.M{"$regex": f.Name}
	}
	if f.TeamOwner != "" {
		query["teamowner"] = f.TeamOwner
	}
	if f.Platform != "" {
		query["framework"] = f.Platform
	}
	if f.UserOwner != "" {
		query["owner"] = f.UserOwner
	}
	if f.Locked {
		query["lock.locked"] = true
	}
	return query
}

// List returns the list of apps that the given user has access to.
//
// If the user does not have access to any app, this function returns an empty
// list and a nil error.
//
// The list can be filtered through the filter parameter.
func List(u *auth.User, filter *Filter) ([]App, error) {
	var apps []App
	conn, err := db.Conn()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	query := filter.Query()
	if u == nil || u.IsAdmin() {
		if err := conn.Apps().Find(query).All(&apps); err != nil {
			return []App{}, err
		}
		return apps, nil
	}
	ts, err := u.Teams()
	if err != nil {
		return []App{}, err
	}
	teams := auth.GetTeamsNames(ts)
	query["teams"] = bson.M{"$in": teams}
	if err := conn.Apps().Find(query).All(&apps); err != nil {
		return []App{}, err
	}
	return apps, nil
}

// Swap calls the Provisioner.Swap.
// And updates the app.CName in the database.
func Swap(app1, app2 *App) error {
	err := Provisioner.Swap(app1, app2)
	if err != nil {
		return err
	}
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	app1.CName, app2.CName = app2.CName, app1.CName
	updateCName := func(app *App) error {
		app.Ip, err = Provisioner.Addr(app)
		if err != nil {
			return err
		}
		return conn.Apps().Update(
			bson.M{"name": app.Name},
			bson.M{"$set": bson.M{"cname": app.CName, "ip": app.Ip}},
		)
	}
	err = updateCName(app1)
	if err != nil {
		return err
	}
	return updateCName(app2)
}

// Start starts the app calling the provisioner.Start method and
// changing the units state to StatusStarted.
func (app *App) Start(w io.Writer, process string) error {
	msg := fmt.Sprintf("\n ---> Starting the process %q\n", process)
	if process == "" {
		msg = fmt.Sprintf("\n ---> Starting the app %q\n", app.Name)
	}
	log.Write(w, []byte(msg))
	err := Provisioner.Start(app, process)
	if err != nil {
		log.Errorf("[start] error on start the app %s - %s", app.Name, err)
		return err
	}
	return nil
}

func (app *App) SetUpdatePlatform(check bool) error {
	conn, err := db.Conn()
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Apps().Update(
		bson.M{"name": app.Name},
		bson.M{"$set": bson.M{"updateplatform": check}},
	)
}

func (app *App) GetUpdatePlatform() bool {
	return app.UpdatePlatform
}

func (app *App) RegisterUnit(unitId string, customData map[string]interface{}) error {
	units, err := app.Units()
	if err != nil {
		return err
	}
	for _, unit := range units {
		if strings.HasPrefix(unit.ID, unitId) {
			return Provisioner.RegisterUnit(unit, customData)
		}
	}
	return provision.ErrUnitNotFound
}

func (app *App) GetRouter() (string, error) {
	return app.Plan.getRouter()
}

func (app *App) MetricEnvs() map[string]string {
	return Provisioner.MetricEnvs(app)
}

func (app *App) Shell(opts provision.ShellOptions) error {
	opts.App = app
	return Provisioner.Shell(opts)
}

type ProcfileError struct {
	yamlErr error
}

func (e *ProcfileError) Error() string {
	return fmt.Sprintf("error parsing Procfile: %s", e.yamlErr)
}

type RebuildRoutesResult struct {
	Added   []string
	Removed []string
}

func (app *App) RebuildRoutes() (*RebuildRoutesResult, error) {
	routerName, err := app.GetRouter()
	if err != nil {
		return nil, err
	}
	r, err := router.Get(routerName)
	if err != nil {
		return nil, err
	}
	err = r.AddBackend(app.Name)
	if err != nil && err != router.ErrBackendExists {
		return nil, err
	}
	var newAddr string
	if newAddr, err = r.Addr(app.GetName()); err == nil && newAddr != app.Ip {
		var conn *db.Storage
		conn, err = db.Conn()
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		err = conn.Apps().Update(bson.M{"name": app.Name}, bson.M{"$set": bson.M{"ip": newAddr}})
		if err != nil {
			return nil, err
		}
	}
	for _, cname := range app.CName {
		err := r.SetCName(cname, app.Name)
		if err != nil && err != router.ErrCNameExists {
			return nil, err
		}
	}
	oldRoutes, err := r.Routes(app.GetName())
	if err != nil {
		return nil, err
	}
	expectedMap := make(map[string]*url.URL)
	units, err := Provisioner.RoutableUnits(app)
	if err != nil {
		return nil, err
	}
	for _, unit := range units {
		expectedMap[unit.Address.String()] = unit.Address
	}
	var toRemove []*url.URL
	for _, url := range oldRoutes {
		if _, isPresent := expectedMap[url.String()]; isPresent {
			delete(expectedMap, url.String())
		} else {
			toRemove = append(toRemove, url)
		}
	}
	var result RebuildRoutesResult
	for _, toAddUrl := range expectedMap {
		err := r.AddRoute(app.GetName(), toAddUrl)
		if err != nil {
			return nil, err
		}
		result.Added = append(result.Added, toAddUrl.String())
	}
	for _, toRemoveUrl := range toRemove {
		err := r.RemoveRoute(app.GetName(), toRemoveUrl)
		if err != nil {
			return nil, err
		}
		result.Removed = append(result.Removed, toRemoveUrl.String())
	}
	return &result, nil
}
