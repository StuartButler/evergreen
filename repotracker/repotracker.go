package repotracker

import (
	"context"
	"fmt"
	"time"

	"github.com/evergreen-ci/evergreen"
	"github.com/evergreen-ci/evergreen/db"
	"github.com/evergreen-ci/evergreen/model"
	"github.com/evergreen-ci/evergreen/model/event"
	"github.com/evergreen-ci/evergreen/model/user"
	"github.com/evergreen-ci/evergreen/model/version"
	"github.com/evergreen-ci/evergreen/thirdparty"
	"github.com/evergreen-ci/evergreen/util"
	"github.com/evergreen-ci/evergreen/validator"
	"github.com/mongodb/grip"
	"github.com/mongodb/grip/level"
	"github.com/mongodb/grip/message"
	"github.com/pkg/errors"
	yaml "gopkg.in/yaml.v2"
)

const (
	// determines the default maximum number of revisions to fetch for a newly tracked repo
	// if not specified in configuration file
	DefaultNumNewRepoRevisionsToFetch = 200
	DefaultMaxRepoRevisionsToSearch   = 50
	DefaultNumConcurrentRequests      = 10
)

// RepoTracker is used to manage polling repository changes and storing such
// changes. It contains a number of interfaces that specify behavior required by
// client implementations
type RepoTracker struct {
	*evergreen.Settings
	*model.ProjectRef
	RepoPoller
}

type VersionErrors struct {
	Errors   []string
	Warnings []string
}

// The RepoPoller interface specifies behavior required of all repository poller
// implementations
type RepoPoller interface {
	// Fetches the contents of a remote repository's configuration data as at
	// the given revision.
	GetRemoteConfig(ctx context.Context, revision string) (*model.Project, error)

	// Fetches a list of all filepaths modified by a given revision.
	GetChangedFiles(ctx context.Context, revision string) ([]string, error)

	// Fetches all changes since the 'revision' specified - with the most recent
	// revision appearing as the first element in the slice.
	//
	// 'maxRevisionsToSearch' determines the maximum number of revisions we
	// allow to search through - in order to find 'revision' - before we give
	// up. A value <= 0 implies we allow to search through till we hit the first
	// revision for the project.
	GetRevisionsSince(sinceRevision string, maxRevisions int) ([]model.Revision, error)
	// Fetches the most recent 'numNewRepoRevisionsToFetch' revisions for a
	// project - with the most recent revision appearing as the first element in
	// the slice.
	GetRecentRevisions(numNewRepoRevisionsToFetch int) ([]model.Revision, error)
}

type projectConfigError struct {
	Errors   []string
	Warnings []string
}

func (p projectConfigError) Error() string {
	return "Invalid project configuration"
}

// The FetchRevisions method is used by a RepoTracker to run the pipeline for
// tracking repositories. It performs everything from polling the repository to
// persisting any changes retrieved from the repository reference.
func (repoTracker *RepoTracker) FetchRevisions(ctx context.Context) error {
	settings := repoTracker.Settings
	projectRef := repoTracker.ProjectRef
	projectIdentifier := projectRef.String()

	if !projectRef.Enabled {
		// this is somewhat belt-and-suspenders, as the
		// repotracker runner process doesn't run for disabled
		// proejcts.
		grip.Info(message.Fields{
			"message": "skip disabled project",
			"project": projectRef,
			"runner":  RunnerName,
		})
		return nil
	}

	repository, err := model.FindRepository(projectIdentifier)
	if err != nil {
		return errors.Wrapf(err, "error finding repository '%v'", projectIdentifier)
	}

	var revisions []model.Revision
	var lastRevision string

	if repository != nil {
		lastRevision = repository.LastRevision
	}

	if lastRevision == "" {
		numRevisions := settings.RepoTracker.NumNewRepoRevisionsToFetch
		if numRevisions <= 0 {
			numRevisions = DefaultNumNewRepoRevisionsToFetch
		}
		// if this is the first time we're running the tracker for this project,
		// fetch the most recent `numNewRepoRevisionsToFetch` revisions
		grip.Debug(message.Fields{
			"runner":  RunnerName,
			"project": projectRef,
			"message": "no last recorded revision, using most recent revisions",
			"number":  numRevisions,
		})
		revisions, err = repoTracker.GetRecentRevisions(numRevisions)
	} else {
		grip.Debug(message.Fields{
			"message":  "found last recorded revision",
			"project":  projectRef,
			"runner":   RunnerName,
			"revision": lastRevision,
		})
		// if the projectRef has a repotracker error then don't get the revisions
		if projectRef.RepotrackerError != nil {
			if projectRef.RepotrackerError.Exists {
				grip.Warning(message.Fields{
					"runner":  RunnerName,
					"message": "repotracker error for base revision",
					"project": projectRef,
					"path":    fmt.Sprintf("%s/%s:%s", projectRef.Owner, projectRef.Repo, projectRef.Branch),
				})
				return nil
			}
		}
		max := settings.RepoTracker.MaxRepoRevisionsToSearch
		if max <= 0 {
			max = DefaultMaxRepoRevisionsToSearch
		}
		revisions, err = repoTracker.GetRevisionsSince(lastRevision, max)
	}

	if err != nil {
		grip.Error(message.WrapError(err, message.Fields{
			"message": "problem fetching revisions for repository",
			"runner":  RunnerName,
			"project": projectRef.Identifier,
		}))
		return nil
	}

	if len(revisions) > 0 {
		var lastVersion *version.Version
		lastVersion, err = repoTracker.StoreRevisions(ctx, revisions)
		if err != nil {
			grip.Error(message.WrapError(err, message.Fields{
				"message": "problem sorting revisions for repository",
				"runner":  RunnerName,
				"project": projectRef,
			}))
			return errors.WithStack(err)
		}
		err = model.UpdateLastRevision(lastVersion.Identifier, lastVersion.Revision)
		if err != nil {
			grip.Error(message.WrapError(err, message.Fields{
				"message": "problem updating last revision for repository",
				"project": projectRef,
				"runner":  RunnerName,
			}))
			return errors.WithStack(err)
		}
	}

	if err := model.DoProjectActivation(projectIdentifier); err != nil {
		grip.Error(message.WrapError(err, message.Fields{
			"message": "problem activating recent commit for project",
			"project": projectIdentifier,
			"runner":  RunnerName,
			"mode":    "ingestion",
		}))

		return errors.WithStack(err)
	}

	return nil
}

// Constructs all versions stored from recent repository revisions
// The additional complexity is due to support for project modifications on patch builds.
// We need to parse the remote config as it existed when each revision was created.
// The return value is the most recent version created as a result of storing the revisions.
// This function is idempotent with regard to storing the same version multiple times.
func (repoTracker *RepoTracker) StoreRevisions(ctx context.Context, revisions []model.Revision) (newestVersion *version.Version, err error) {
	defer func() {
		if newestVersion != nil {
			// Fetch the updated version doc, so that we include buildvariants in the result
			newestVersion, err = version.FindOne(version.ById(newestVersion.Id))
		}
	}()
	ref := repoTracker.ProjectRef
	for i := len(revisions) - 1; i >= 0; i-- {
		revision := revisions[i].Revision
		grip.Infof("Processing revision %s in project %s", revision, ref.Identifier)

		// We check if the version exists here so we can avoid fetching the github config unnecessarily
		existingVersion, err := version.FindOne(version.ByProjectIdAndRevision(ref.Identifier, revisions[i].Revision))
		grip.Error(message.WrapError(err, message.Fields{
			"message":  "problem looking up version for project",
			"runner":   RunnerName,
			"project":  ref.Identifier,
			"revision": revision,
		}))

		if existingVersion != nil {
			grip.Info(message.Fields{
				"message":  "skipping creating version because it already exists",
				"runner":   RunnerName,
				"project":  ref.Identifier,
				"revision": revision,
			})
			// We bind newestVersion here since we still need to return the most recent
			// version, even if it already exists
			newestVersion = existingVersion
			continue
		}

		var versionErrs *VersionErrors
		project, err := repoTracker.GetProjectConfig(ctx, revision)
		if err != nil {
			// this is an error that implies the file is invalid - create a version and store the error
			projErr, isProjErr := err.(projectConfigError)
			if isProjErr {
				versionErrs = &VersionErrors{
					Warnings: projErr.Warnings,
					Errors:   projErr.Errors,
				}
				if len(versionErrs.Errors) > 0 {
					stubVersion, dbErr := shellVersionFromRevision(ref, revisions[i])
					if dbErr != nil {
						grip.Error(message.WrapError(dbErr, message.Fields{
							"message":  "error creating shell version",
							"runner":   RunnerName,
							"project":  ref.Identifier,
							"revision": revision,
						}))
					}
					stubVersion.Errors = versionErrs.Errors
					stubVersion.Warnings = versionErrs.Warnings
					err = stubVersion.Insert()
					grip.Error(message.WrapError(err, message.Fields{
						"message":  "error inserting shell version",
						"runner":   RunnerName,
						"project":  ref.Identifier,
						"revision": revision,
					}))
					newestVersion = stubVersion
					continue
				}
			} else {
				grip.Error(message.WrapError(err, message.Fields{
					"message":  "error getting project config",
					"runner":   RunnerName,
					"project":  ref.Identifier,
					"revision": revision,
				}))
				return nil, err
			}
		}

		// "Ignore" a version if all changes are to ignored files
		var ignore bool
		if len(project.Ignore) > 0 {
			var filenames []string
			filenames, err = repoTracker.GetChangedFiles(ctx, revision)
			if err != nil {
				grip.Error(message.WrapError(err, message.Fields{
					"message":  "error checking GitHub for ignored files",
					"runner":   RunnerName,
					"project":  ref.Identifier,
					"revision": revision,
				}))
				continue
			}
			if project.IgnoresAllFiles(filenames) {
				ignore = true
			}
		}

		v, err := CreateVersionFromConfig(ref, project, &revisions[i], ignore, versionErrs)
		if err != nil {
			grip.Error(message.WrapError(err, message.Fields{
				"message":  "error creating version",
				"runner":   RunnerName,
				"project":  ref.Identifier,
				"revision": revision,
			}))
			continue
		}
		if err = addBuildBreakSubscriptions(v, ref); err != nil {
			grip.Error(message.WrapError(err, message.Fields{
				"message":  "error creating build break subscriptions",
				"runner":   RunnerName,
				"project":  ref.Identifier,
				"revision": revision,
			}))
			continue
		}

		newestVersion = v
	}
	return newestVersion, nil
}

// GetProjectConfig fetches the project configuration for a given repository
// returning a remote config if the project references a remote repository
// configuration file - via the Identifier. Otherwise it defaults to the local
// project file. An erroneous project file may be returned along with an error.
func (repoTracker *RepoTracker) GetProjectConfig(ctx context.Context, revision string) (*model.Project, error) {
	projectRef := repoTracker.ProjectRef
	if projectRef.LocalConfig != "" {
		// return the Local config from the project Ref.
		p, err := model.FindProject("", projectRef)
		return p, err
	}
	project, err := repoTracker.GetRemoteConfig(ctx, revision)
	if err != nil {
		// Only create a stub version on API request errors that pertain
		// to actually fetching a config. Those errors currently include:
		// thirdparty.APIRequestError, thirdparty.FileNotFoundError and
		// thirdparty.YAMLFormatError
		_, apiReqErr := err.(thirdparty.APIRequestError)
		_, ymlFmtErr := err.(thirdparty.YAMLFormatError)
		_, noFileErr := err.(thirdparty.FileNotFoundError)
		if apiReqErr || noFileErr || ymlFmtErr {
			// If there's an error getting the remote config, e.g. because it
			// does not exist, we treat this the same as when the remote config
			// is invalid - but add a different error message
			msg := message.ConvertToComposer(level.Error, message.Fields{
				"message":  "problem finding project configuration",
				"runner":   RunnerName,
				"project":  projectRef.Identifier,
				"revision": revision,
				"path":     projectRef.RemotePath,
			})

			grip.Error(message.WrapError(err, msg))
			return nil, projectConfigError{Errors: []string{msg.String()}, Warnings: nil}
		}
		// If we get here then we have an infrastructural error - e.g.
		// a thirdparty.APIUnmarshalError (indicating perhaps an API has
		// changed), a thirdparty.ResponseReadError(problem reading an
		// API response) or a thirdparty.APIResponseError (nil API
		// response) - or encountered a problem in fetching a local
		// configuration file. At any rate, this is bad enough that we
		// want to send a notification instead of just creating a stub
		// version.
		var lastRevision string
		repository, fErr := model.FindRepository(projectRef.Identifier)
		if fErr != nil || repository == nil {
			grip.Error(message.WrapError(fErr, message.Fields{
				"message": "problem finding repository",
				"project": projectRef.Identifier,
				"runner":  RunnerName,
			}))
		} else {
			lastRevision = repository.LastRevision
		}

		// this used to send email, but it happens so
		// infrequently, and mail is a bad format for this.
		grip.Critical(message.WrapError(err, message.Fields{
			"message":      "repotracker configuration problem",
			"project":      projectRef.Identifier,
			"runner":       RunnerName,
			"lastRevision": lastRevision,
		}))

		return nil, err
	}
	return project, nil
}

func addBuildBreakSubscriptions(v *version.Version, projectRef *model.ProjectRef) error {
	subscriptionBase := event.Subscription{
		ResourceType: event.ResourceTypeVersion,
		Trigger:      "build-break",
		Selectors: []event.Selector{
			{
				Type: "object",
				Data: "task",
			},
			{
				Type: "project",
				Data: projectRef.Identifier,
			},
			{
				Type: "requester",
				Data: evergreen.RepotrackerVersionRequester,
			},
		},
	}
	subscribers := []event.Subscriber{}

	// if the commit author has subscribed to build break notifications,
	// send it to the comitter, but not the admins
	catcher := grip.NewSimpleCatcher()
	if v.AuthorID != "" {
		author, err := user.FindOne(user.ById(v.AuthorID))
		if err != nil {
			catcher.Add(errors.Wrap(err, "unable to retrieve user"))
		} else if author.Settings.Notifications.BuildBreakID != "" {
			return nil
		}
	}

	// Only send to admins if the admins have enabled build break notifications
	if !projectRef.NotifyOnBuildFailure {
		return nil
	}
	// if the project has build break notifications, subscribe admins if no one subscribed
	for _, admin := range projectRef.Admins {
		subscriber, err := makeBuildBreakSubscriber(admin)
		if err != nil {
			catcher.Add(err)
			continue
		}
		if subscriber != nil {
			subscribers = append(subscribers, *subscriber)
		}
	}

	for _, subscriber := range subscribers {
		newSubscription := subscriptionBase
		newSubscription.Subscriber = subscriber
		catcher.Add(newSubscription.Upsert())
	}
	return catcher.Resolve()
}

func makeBuildBreakSubscriber(userID string) (*event.Subscriber, error) {
	u, err := user.FindOne(user.ById(userID))
	if err != nil {
		return nil, errors.Wrap(err, "unable to find user")
	}
	if u == nil {
		return nil, errors.Errorf("user %s does not exist", userID)
	}
	var subscriber *event.Subscriber
	preference := u.Settings.Notifications.BuildBreak
	if preference != "" {
		subscriber = &event.Subscriber{
			Type: string(preference),
		}
		if preference == user.PreferenceEmail {
			subscriber.Target = u.Email()
		} else if preference == user.PreferenceSlack {
			subscriber.Target = u.Settings.SlackUsername
		} else {
			return nil, errors.Errorf("invalid subscription preference for build break: %s", preference)
		}
	}

	return subscriber, nil
}

func CreateVersionFromConfig(ref *model.ProjectRef, config *model.Project, rev *model.Revision, ignore bool, versionErrs *VersionErrors) (*version.Version, error) {
	if ref == nil || config == nil {
		return nil, errors.New("project ref and project cannot be nil")
	}

	// create a version document
	v, err := shellVersionFromRevision(ref, *rev)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create shell version")
	}
	if err = sanityCheckOrderNum(v.RevisionOrderNumber, ref.Identifier, rev.Revision); err != nil {
		return nil, errors.Wrap(err, "inconsistent version order")
	}
	configYaml, err := yaml.Marshal(config)
	if err != nil {
		return nil, errors.Wrap(err, "error marshaling config")
	}
	v.Config = string(configYaml)
	v.Ignored = ignore

	// validate the project
	verrs, err := validator.CheckProjectSyntax(config)
	if err != nil {
		return nil, errors.Wrap(err, "error validating project")
	}
	if len(verrs) > 0 || versionErrs != nil {
		// We have syntax errors in the project.
		// Format them, as we need to store + display them to the user
		var projectErrors, projectWarnings []string
		for _, e := range verrs {
			if e.Level == validator.Warning {
				projectWarnings = append(projectWarnings, e.Error())
			} else {
				projectErrors = append(projectErrors, e.Error())
			}
		}
		v.Warnings = projectWarnings
		v.Errors = projectErrors
		if versionErrs != nil && versionErrs.Warnings != nil {
			v.Warnings = append(v.Warnings, versionErrs.Warnings...)
		}
		if versionErrs != nil && versionErrs.Errors != nil {
			v.Errors = append(v.Errors, versionErrs.Errors...)
		}
		if len(v.Errors) > 0 {
			return v, errors.Wrap(v.Insert(), "error inserting version")
		}
	}

	return v, errors.Wrap(createVersionItems(v, ref, config), "error creating version items")
}

// shellVersionFromRevision populates a new Version with metadata from a model.Revision.
// Does not populate its config or store anything in the database.
func shellVersionFromRevision(ref *model.ProjectRef, rev model.Revision) (*version.Version, error) {
	u, err := user.FindByGithubUID(rev.AuthorGithubUID)
	grip.Error(message.WrapError(err, message.Fields{
		"message": fmt.Sprintf("failed to fetch everg user with Github UID %d", rev.AuthorGithubUID),
	}))

	number, err := model.GetNewRevisionOrderNumber(ref.Identifier)
	if err != nil {
		return nil, err
	}
	v := &version.Version{
		Author:              rev.Author,
		AuthorEmail:         rev.AuthorEmail,
		Branch:              ref.Branch,
		CreateTime:          rev.CreateTime,
		Id:                  util.CleanName(fmt.Sprintf("%v_%v", ref.String(), rev.Revision)),
		Identifier:          ref.Identifier,
		Message:             rev.RevisionMessage,
		Owner:               ref.Owner,
		RemotePath:          ref.RemotePath,
		Repo:                ref.Repo,
		RepoKind:            ref.RepoKind,
		Requester:           evergreen.RepotrackerVersionRequester,
		Revision:            rev.Revision,
		Status:              evergreen.VersionCreated,
		RevisionOrderNumber: number,
	}
	if u != nil {
		v.AuthorID = u.Id
	}
	return v, nil
}

// Verifies that the given revision order number is higher than the latest number stored for the project.
func sanityCheckOrderNum(revOrderNum int, projectId, revision string) error {
	latest, err := version.FindOne(version.ByMostRecentSystemRequester(projectId))
	if err != nil || latest == nil {
		return errors.Wrap(err, "Error getting latest version")
	}

	if latest.Revision == revision {
		grip.Critical(message.Fields{
			"project":   projectId,
			"runner":    RunnerName,
			"message":   "attempting to add a duplicate version",
			"rev_num":   revOrderNum,
			"revision":  revision,
			"latest_id": latest.Id,
		})
		return errors.New("refusing to add a new version with a duplicate revision id")
	}

	// When there are no versions in the db yet, sanity check is moot
	if latest != nil {
		if revOrderNum <= latest.RevisionOrderNumber {
			return errors.Errorf("Commit order number isn't greater than last stored version's: %v <= %v",
				revOrderNum, latest.RevisionOrderNumber)
		}
	}
	return nil
}

// createVersionItems populates and stores all the tasks and builds for a version according to
// the given project config.
func createVersionItems(v *version.Version, ref *model.ProjectRef, project *model.Project) error {
	// generate all task Ids so that we can easily reference them for dependencies
	taskIds := model.NewTaskIdTable(project, v)

	// create all builds for the version
	for _, buildvariant := range project.BuildVariants {
		if buildvariant.Disabled {
			continue
		}

		buildId, err := model.CreateBuildFromVersion(project, v, taskIds, buildvariant.Name, false, nil, nil, "")
		if err != nil {
			return errors.WithStack(err)
		}

		lastActivated, err := version.FindOne(version.ByLastVariantActivation(ref.Identifier, buildvariant.Name))
		if err != nil {
			return errors.Wrap(err, "problem getting activatation time for variant")
		}

		var lastActivation *time.Time
		if lastActivated != nil {
			for _, buildStatus := range lastActivated.BuildVariants {
				if buildStatus.BuildVariant == buildvariant.Name && buildStatus.Activated {
					lastActivation = &buildStatus.ActivateAt
					break
				}
			}
		}

		var activateAt time.Time
		if lastActivation == nil {
			// if we don't have a last activation time then prepare to activate it immediately.
			activateAt = time.Now()
		} else {
			activateAt = lastActivation.Add(time.Minute * time.Duration(ref.GetBatchTime(&buildvariant)))
		}

		grip.Debug(message.Fields{
			"message": "activating build",
			"name":    buildvariant.Name,
			"project": ref.Identifier,
			"version": v.Id,
			"time":    activateAt,
			"runner":  RunnerName,
		})
		v.BuildIds = append(v.BuildIds, buildId)
		v.BuildVariants = append(v.BuildVariants, version.BuildStatus{
			BuildVariant: buildvariant.Name,
			Activated:    false,
			ActivateAt:   activateAt,
			BuildId:      buildId,
		})
	}

	err := v.Insert()
	if err != nil && !db.IsDuplicateKey(err) {
		grip.Error(message.WrapError(err, message.Fields{
			"message": "problem inserting version",
			"runner":  RunnerName,
			"id":      v.Id,
		}))
		for _, buildStatus := range v.BuildVariants {
			if buildErr := model.DeleteBuild(buildStatus.BuildId); buildErr != nil {
				grip.Error(message.WrapError(buildErr, message.Fields{
					"message":    "issue deleting build",
					"runner":     RunnerName,
					"version_id": v.Id,
					"build_id":   buildStatus.BuildId,
				}))
			}
		}
		return errors.WithStack(err)
	}
	return nil
}
