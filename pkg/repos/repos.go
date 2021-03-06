//Package repos provides an interface for processing repositories.
//go:generate mockgen -destination=../mocks/repos/mockRepos.go -package=mocks -source=repos.go . Repo,Repos
package repos

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"

	"github.com/fidelity/kraan/pkg/internal/tarconsumer"
)

var (
	DefaultRootPath = "/data"
	DefaultHostName = ""
	DefaultTimeOut  = 15 * time.Second
)

// Repos defines the interface for managing multiple instances of repository and revision data.
type Repos interface {
	Add(srcRepo *sourcev1.GitRepository) Repo
	Get(name string) Repo
	Delete(name string)
	List() map[string]Repo
	SetRootPath(path string)
	SetHostName(hostName string)
	SetTimeOut(timeOut time.Duration)
	SetHTTPClient(client *http.Client)
	PathKey(srcRepo *sourcev1.GitRepository) string
}

// reposData hold data about all repositories.
type reposData struct {
	repos        map[string]Repo
	ctx          context.Context
	log          logr.Logger
	rootPath     string
	hostName     string
	timeOut      time.Duration
	client       *http.Client
	Repos        `json:"-"`
	sync.RWMutex `json:"-"`
}

// NewRepos creates a repos object.
func NewRepos(ctx context.Context, log logr.Logger) Repos {
	return &reposData{
		repos:    make(map[string]Repo, 1),
		ctx:      ctx,
		log:      log,
		rootPath: DefaultRootPath,
		hostName: DefaultHostName,
		timeOut:  DefaultTimeOut,
		client:   &http.Client{},
	}
}

func (r *reposData) PathKey(repo *sourcev1.GitRepository) string {
	return fmt.Sprintf("%s/%s", repo.GetNamespace(), repo.GetName())
}

func (r *reposData) SetRootPath(path string) {
	r.rootPath = path
}

func (r *reposData) SetHostName(hostName string) {
	r.hostName = hostName
}

func (r *reposData) SetTimeOut(timeOut time.Duration) {
	r.timeOut = timeOut
}

func (r *reposData) SetHTTPClient(client *http.Client) {
	r.client = client
}

// List returns a map of repos keyed by repo label
func (r *reposData) List() map[string]Repo {
	r.RLock()
	defer r.RUnlock()
	return r.repos
}

// Get returns a worker or nil if not present
func (r *reposData) Get(name string) Repo {
	r.RLock()
	defer r.RUnlock()
	if repo, found := r.repos[name]; found {
		return repo
	}
	return nil
}

// Add adds a repo to the map of active repos
func (r *reposData) Add(repo *sourcev1.GitRepository) Repo {
	r.Lock()
	defer r.Unlock()
	key := r.PathKey(repo)
	if _, found := r.repos[key]; !found {
		r.repos[key] = r.newRepo(key, repo)
	}
	return r.repos[key]
}

// Delete deletes a repo from the map of active repos
func (r *reposData) Delete(name string) {
	r.Lock()
	defer r.Unlock()
	if _, found := r.repos[name]; found {
		delete(r.repos, name)
	}
}

// Repo defines the interface for managing repository and revision data.
type Repo interface {
	GetSourceName() string
	GetSourceNameSpace() string
	SyncRepo() error
	LinkData(layerPath, sourcePath string) error
	GetGitRepo() *sourcev1.GitRepository
	GetPath() string
	GetDataPath() string
	GetLoadPath() string
	SetHostName(hostName string)
	SetHTTPClient(client *http.Client)
	SetTarConsumer(tarConsumer tarconsumer.TarConsumer)
	fetchArtifact(ctx context.Context) error
}

// repoData hold data about a repository.
type repoData struct {
	ctx          context.Context
	log          logr.Logger
	client       *http.Client
	hostName     string
	dataPath     string
	loadPath     string
	path         string
	repo         *sourcev1.GitRepository
	tarConsumer  tarconsumer.TarConsumer
	Repo         `json:"-"`
	sync.RWMutex `json:"-"`
	syncLock     sync.RWMutex
}

// newRepo creates a repo.
func (r *reposData) newRepo(path string, sourceRepo *sourcev1.GitRepository) Repo {
	repo := &repoData{
		ctx:         r.ctx,
		log:         r.log,
		client:      r.client,
		hostName:    r.hostName,
		dataPath:    fmt.Sprintf("%s/%s", r.rootPath, path),
		loadPath:    fmt.Sprintf("%s/load/%s", r.rootPath, path),
		path:        path,
		repo:        sourceRepo,
		tarConsumer: tarconsumer.NewTarConsumer(r.ctx, r.client, sourceRepo.Status.Artifact.URL),
	}
	return repo
}

func (r *repoData) GetGitRepo() *sourcev1.GitRepository {
	r.RLock()
	defer r.RUnlock()
	return r.repo
}

func (r *repoData) GetPath() string {
	return r.path
}

func (r *repoData) GetDataPath() string {
	return r.dataPath
}

func (r *repoData) GetLoadPath() string {
	return r.loadPath
}

func (r *repoData) SetHostName(hostName string) {
	r.hostName = hostName
}

func (r *repoData) SetHTTPClient(client *http.Client) {
	r.client = client
}

func (r *repoData) SetTarConsumer(tarConsumer tarconsumer.TarConsumer) {
	r.tarConsumer = tarConsumer
}

func (r *repoData) GetSourceName() string {
	r.RLock()
	defer r.RUnlock()
	return r.repo.GetName()
}

func (r *repoData) GetSourceNameSpace() string {
	r.RLock()
	defer r.RUnlock()
	return r.repo.GetNamespace()
}

func (r *repoData) LinkData(layerPath, sourcePath string) error {
	r.Lock()
	defer r.Unlock()
	addonsPath := fmt.Sprintf("%s/%s", r.GetDataPath(), sourcePath)
	if err := isExistingDir(addonsPath); err != nil {
		return errors.Wrap(err, "failed, target directory does not exist")
	}
	layerPathParts := strings.Split(layerPath, "/")
	layerPathDir := strings.Join(layerPathParts[:len(layerPathParts)-1], "/")

	if err := os.MkdirAll(layerPathDir, os.ModePerm); err != nil {
		return errors.Wrapf(err, "failed to make directory: %s", layerPathDir)
	}
	if _, err := os.Lstat(layerPath); err == nil {
		if e := os.RemoveAll(layerPath); e != nil {
			return errors.Wrapf(err, "failed to remove link: %s", layerPath)
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	if err := os.Symlink(addonsPath, layerPath); err != nil {
		return errors.Wrapf(err, "failed to create link: %s", layerPath)
	}
	return nil
}

/*
Copyright 2020 The Flux CD contributors.

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

func (r *repoData) SyncRepo() error {
	r.syncLock.Lock()
	defer r.syncLock.Unlock()
	ctx, cancel := context.WithTimeout(r.ctx, DefaultTimeOut)
	defer cancel()

	r.log.Info("New revision detected", "kind", "gitrepositories.source.toolkit.fluxcd.io",
		"namespace", r.GetSourceNameSpace(), "name", r.GetSourceName(), "revision", r.repo.Status.Artifact.Revision)

	if _, err := os.Stat(r.loadPath); os.IsNotExist(err) {
		if e := os.RemoveAll(r.loadPath); e != nil {
			return errors.Wrap(e, "failed to remove directory")
		}
	} else if err != nil {
		return err
	}
	if err := os.MkdirAll(r.loadPath, os.ModePerm); err != nil {
		return errors.Wrapf(err, "failed to make directory: %s", r.loadPath)
	}

	// download and extract artifact
	if err := r.fetchArtifact(ctx); err != nil {
		return errors.Wrap(err, "failed to fetch repository tar file from source controller")
	}

	if err := os.MkdirAll(r.dataPath, os.ModePerm); err != nil {
		return errors.Wrapf(err, "failed to make directory: %s", r.dataPath)
	}

	r.Lock()
	defer r.Unlock()
	if e := os.RemoveAll(r.dataPath); e != nil {
		return errors.Wrapf(e, "failed to remove data path: %s", r.dataPath)
	}
	if err := os.Rename(r.loadPath, r.dataPath); err != nil {
		return errors.Wrapf(err, "failed to rename load path: %s", r.loadPath)
	}
	return nil
}

func (r *repoData) fetchArtifact(ctx context.Context) error {
	repo := r.repo
	if repo.Status.Artifact == nil {
		return fmt.Errorf("repository %s does not contain an artifact", r.path)
	}

	url := repo.Status.Artifact.URL

	if r.hostName != "" {
		url = fmt.Sprintf("http://%s/gitrepository/%s/%s/latest.tar.gz", r.hostName, repo.Namespace, repo.Name)
	}

	r.tarConsumer.SetURL(url)

	tar, err := r.tarConsumer.GetTar(ctx)
	if err != nil {
		return errors.WithMessagef(err, "failed to download artifact from %s", url)
	}
	// Debugging for unzip error
	r.log.V(2).Info("tar data", "length", len(tar),
		"kind", "gitrepositories.source.toolkit.fluxcd.io", "namespace", r.GetSourceNameSpace(), "name", r.GetSourceName())

	if err := tarconsumer.UnpackTar(tar, r.GetLoadPath()); err != nil {
		return errors.WithMessage(err, "faild to untar artifact")
	}

	return nil
}

func isExistingDir(dataPath string) error {
	info, err := os.Stat(dataPath)
	if os.IsNotExist(err) {
		return errors.Wrapf(err, "failed to stat: %s", dataPath)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("addons Data path: %s, is not a directory", dataPath)
	}

	return nil
}
