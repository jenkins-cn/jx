package cmd

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	neturl "net/url"

	"github.com/jenkins-x/golang-jenkins"
	"github.com/jenkins-x/jx/pkg/gits"
	"github.com/jenkins-x/jx/pkg/jenkins"
	"github.com/jenkins-x/jx/pkg/jx/cmd/templates"
	cmdutil "github.com/jenkins-x/jx/pkg/jx/cmd/util"
	"github.com/jenkins-x/jx/pkg/util"
	"github.com/spf13/cobra"
	"gopkg.in/AlecAivazis/survey.v1"
	gitcfg "gopkg.in/src-d/go-git.v4/config"
	"strings"
)

const (
	maximumNewDirectoryAttempts = 1000

	DefaultWritePermissions = 0760

	defaultGitIgnoreFile = `
.project
.classpath
.idea
.cache
.DS_Store
*.im?
target
work
`

	// TODO replace with the jx-pipelines-plugin version when its available
	defaultJenkinsfile = `
pipeline {
  agent {
    label "jenkins-maven"
  }

  stages {

    stage('Build Release') {
      steps {
        container('maven') {
          sh "mvn versions:set -DnewVersion=\$(jx-release-version)"
        }
        dir ('./helm/spring-boot-web-example') {
          container('maven') {
            // until kubernetes plugin supports init containers https://github.com/jenkinsci/kubernetes-plugin/pull/229/
            sh 'cp /root/netrc/.netrc ~/.netrc'

            sh "make tag"
          }
        }
        container('maven') {
          sh "mvn clean deploy fabric8:build fabric8:push -Ddocker.push.registry=$JENKINS_X_DOCKER_REGISTRY_SERVICE_HOST:$JENKINS_X_DOCKER_REGISTRY_SERVICE_PORT"
        }
      }
    }
    stage('Deploy Staging') {

      steps {
        dir ('./helm/spring-boot-web-example') {
          container('maven') {
            sh 'make release'
            sh 'helm install . --namespace staging --name example-release'
            sh 'exposecontroller --namespace staging --http' // until we switch to git environments where helm hooks will expose services
          }
        }
      }
    }
  }
}
`
)

type ImportOptions struct {
	CommonOptions

	RepoURL string

	Dir          string
	Organisation string
	Repository   string
	Credentials  string

	Jenkins    *gojenkins.Jenkins
	GitConfDir string
}

var (
	import_long = templates.LongDesc(`
		Imports a git repository or folder into Jenkins X.

		If you specify no other options or arguments then the current directory is imported.
	    Or you can use '--dir' to specify a directory to import.

	    You can specify the git URL as an argument.`)

	import_example = templates.Examples(`
		# Import the current folder
		jx import

		# Import a different folder
		jx import /foo/bar

		# Import a git repository from a URL
		jx import -repo https://github.com/jenkins-x/spring-boot-web-example.git`)
)

func NewCmdImport(f cmdutil.Factory, out io.Writer, errOut io.Writer) *cobra.Command {
	options := &ImportOptions{
		CommonOptions: CommonOptions{
			Factory: f,
			Out:     out,
			Err:     errOut,
		},
	}
	cmd := &cobra.Command{
		Use:     "import",
		Short:   "Imports a local project into Jenkins",
		Long:    import_long,
		Example: import_example,
		Run: func(cmd *cobra.Command, args []string) {
			options.Cmd = cmd
			options.Args = args
			err := options.Run()
			cmdutil.CheckErr(err)
		},
	}
	cmd.Flags().StringVarP(&options.RepoURL, "url", "u", "", "The git clone URL to clone into the current directory and then import")
	cmd.Flags().StringVarP(&options.Organisation, "org", "o", "", "Specify the git provider organisation to import the project into (if it is not already in one)")
	cmd.Flags().StringVarP(&options.Organisation, "name", "n", "", "Specify the git repository name to import the project into (if it is not already in one)")
	cmd.Flags().StringVarP(&options.Credentials, "credentials", "c", "jenkins-x-github", "The Jenkins credentials name used by the job")
	return cmd
}

func (o *ImportOptions) Run() error {
	f := o.Factory
	jenkins, err := f.GetJenkinsClient()
	if err != nil {
		return err
	}
	o.Jenkins = jenkins

	if o.Dir == "" {
		args := o.Args
		if len(args) > 0 {
			o.Dir = args[0]
		} else {
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			o.Dir = dir
		}
	}

	if o.RepoURL != "" {
		err = o.CloneRepository()
		if err != nil {
			return err
		}
	} else {
		err = o.DiscoverGit()
		if err != nil {
			return err
		}

		if o.RepoURL == "" {
			err = o.DiscoverRemoteGitURL()
			if err != nil {
				return err
			}
		}
	}

	err = o.DraftCreate()
	if err != nil {
		return err
	}

	err = o.DefaultJenkinsfile()
	if err != nil {
		return err
	}

	if o.RepoURL == "" {
		err = o.CreateNewRemoteRepository()
		if err != nil {
			return err
		}
	} else {
		err = gits.GitPush(o.Dir)
		if err != nil {
			return err
		}
	}
	return o.DoImport()
}

func (o *ImportOptions) DraftCreate() error {
	args := []string{"create"}

	// TODO this is a workaround of this draft issue:
	// https://github.com/Azure/draft/issues/476
	dir := o.Dir
	pomName := filepath.Join(dir, "pom.xml")
	exists, err := util.FileExists(pomName)
	if err != nil {
		return err
	}
	if exists {
		args = []string{"create", "--pack=github.com/jenkins-x/draft-repo/packs/java"}
	}
	e := exec.Command("draft", args...)
	e.Dir = dir
	e.Stdout = os.Stdout
	e.Stderr = os.Stderr
	err = e.Run()
	if err != nil {
		return fmt.Errorf("Failed to run draft create in %s due to %s", dir, err)
	}
	err = gits.GitAdd(dir, "*")
	if err != nil {
		return err
	}
	err = gits.GitCommitIfChanges(dir, "Draft create")
	if err != nil {
		return err
	}
	return nil
}

func (o *ImportOptions) DefaultJenkinsfile() error {
	dir := o.Dir
	name := filepath.Join(dir, "Jenkinsfile")
	exists, err := util.FileExists(name)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	data := []byte(defaultJenkinsfile)
	err = ioutil.WriteFile(name, data, DefaultWritePermissions)
	if err != nil {
		return fmt.Errorf("Failed to write %s due to %s", name, err)
	}
	err = gits.GitAdd(dir, "Jenkinsfile")
	if err != nil {
		return err
	}
	err = gits.GitCommitIfChanges(dir, "Added default Jenkinsfile pipeline")
	if err != nil {
		return err
	}
	return nil
}

func (o *ImportOptions) CreateNewRemoteRepository() error {
	f := o.Factory
	authConfigSvc, err := f.CreateGitAuthConfigService()
	if err != nil {
		return err
	}
	config := authConfigSvc.Config()

	server, err := config.PickServer("Which git provider?")
	if err != nil {
		return err
	}
	o.Printf("Using git provider %s\n", server.Description())
	url := server.URL
	userAuth, err := config.PickServerUserAuth(url, "Which user name?")
	if err != nil {
		return err
	}
	if userAuth.IsInvalid() {
		tokenUrl := fmt.Sprintf("https://%s/settings/tokens/new?scopes=repo,read:user,user:email,write:repo_hook", url)

		o.Printf("To be able to create a repository on %s we need an API Token\n", server.Label())
		o.Printf("Please click this URL %s\n\n", tokenUrl)
		o.Printf("Then COPY the token and enter in into the form below:\n\n")

		// TODO could we guess this based on the users ~/.git for github?
		defaultUserName := ""
		err = config.EditUserAuth(&userAuth, defaultUserName)
		if err != nil {
			return err
		}

		// TODO lets verify the auth works

		err = authConfigSvc.SaveUserAuth(url, &userAuth)
		if err != nil {
			return fmt.Errorf("Failed to store git auth configuration %s", err)
		}
		if userAuth.IsInvalid() {
			return fmt.Errorf("You did not properly define the user authentication!")
		}
	}

	gitUsername := userAuth.Username
	o.Printf("\n\nAbout to create a repository on server %s with user %s\n", url, gitUsername)

	provider, err := gits.CreateProvider(server, &userAuth)
	if err != nil {
		return err
	}
	org, err := gits.PickOrganisation(provider, gitUsername)
	if err != nil {
		return err
	}
	owner := org
	if org == "" {
		owner = gitUsername
	}
	dir := o.Dir
	repoName := ""
	_, defaultRepoName := filepath.Split(dir)
	prompt := &survey.Input{
		Message: "Enter the new repository name: ",
		Default: defaultRepoName,
	}
	validator := func(val interface{}) error {
		str, ok := val.(string)
		if !ok {
			return fmt.Errorf("Expected string value!")
		}
		if strings.TrimSpace(str) == "" {
			return fmt.Errorf("Repository name is required")
		}
		return provider.ValidateRepositoryName(owner, str)
	}
	err = survey.AskOne(prompt, &repoName, validator)
	if err != nil {
		return err
	}
	if repoName == "" {
		return fmt.Errorf("No repository name specified!")
	}
	fullName := gits.GitRepoName(org, repoName)
	o.Printf("\n\nCreating repository %s\n", fullName)
	privateRepo := false
	repo, err := provider.CreateRepository(org, repoName, privateRepo)
	if err != nil {
		return err
	}
	o.Printf("Created repository at %s\n", repo.HTMLURL)
	o.RepoURL = repo.CloneURL
	pushGitURL, err := gits.GitCreatePushURL(repo.CloneURL, &userAuth)
	if err != nil {
		return err
	}
	err = gits.GitCmd(dir, "remote", "add", "origin", pushGitURL)
	if err != nil {
		return err
	}
	err = gits.GitCmd(dir, "push", "-u", "origin", "master")
	if err != nil {
		return err
	}
	o.Printf("Pushed git repository to %s\n\n", server.Description())
	return nil
}

func (o *ImportOptions) CloneRepository() error {
	url := o.RepoURL
	if url == "" {
		return fmt.Errorf("No git repository URL defined!")
	}
	gitInfo, err := gits.ParseGitURL(url)
	if err != nil {
		return fmt.Errorf("Failed to parse git URL %s due to: %s", url, err)
	}
	cloneDir, err := util.CreateUniqueDirectory(o.Dir, gitInfo.Name, maximumNewDirectoryAttempts)
	if err != nil {
		return err
	}
	err = gits.GitClone(url, cloneDir)
	if err != nil {
		return err
	}
	o.Dir = cloneDir
	return nil
}

// DiscoverGit checks if there is a git clone or prompts the user to import it
func (o *ImportOptions) DiscoverGit() error {
	root, gitConf, err := gits.FindGitConfigDir(o.Dir)
	if err != nil {
		return err
	}
	if root != "" {
		o.Dir = root
		o.GitConfDir = gitConf
		return nil
	}

	dir := o.Dir
	if dir == "" {
		return fmt.Errorf("No directory specified!")
	}

	// lets prompt the user to initiialse the git repository
	o.Printf("The directory %s is not yet using git\n", dir)
	flag := false
	prompt := &survey.Confirm{
		Message: "Would you like to initialise git now?",
		Default: true,
	}
	err = survey.AskOne(prompt, &flag, nil)
	if err != nil {
		return err
	}
	if !flag {
		return fmt.Errorf("Please initialise git yourself then try again")
	}
	err = gits.GitInit(dir)
	if err != nil {
		return err
	}
	o.GitConfDir = filepath.Join(dir, ".git/config")
	err = o.DefaultGitIgnore()
	if err != nil {
		return err
	}
	err = gits.GitAdd(dir, ".gitignore")
	if err != nil {
		return err
	}
	err = gits.GitAdd(dir, "*")
	if err != nil {
		return err
	}

	err = gits.GitStatus(dir)
	if err != nil {
		return err
	}

	message := ""
	messagePrompt := &survey.Input{
		Message: "Commit message: ",
		Default: "Initial import",
	}
	err = survey.AskOne(messagePrompt, &message, nil)
	if err != nil {
		return err
	}
	err = gits.GitCommitIfChanges(dir, message)
	if err != nil {
		return err
	}
	o.Printf("\nGit repository created\n")
	return nil
}

// DiscoverGit checks if there is a git clone or prompts the user to import it
func (o *ImportOptions) DefaultGitIgnore() error {
	name := filepath.Join(o.Dir, ".gitignore")
	exists, err := util.FileExists(name)
	if err != nil {
		return err
	}
	if !exists {
		data := []byte(defaultGitIgnoreFile)
		err = ioutil.WriteFile(name, data, DefaultWritePermissions)
		if err != nil {
			return fmt.Errorf("Failed to write %s due to %s", name, err)
		}
	}
	return nil
}

// DiscoverRemoteGitURL finds the git url by looking in the directory
// and looking for a .git/config file
func (o *ImportOptions) DiscoverRemoteGitURL() error {
	gitConf := o.GitConfDir
	if gitConf == "" {
		return fmt.Errorf("No GitConfDir defined!")
	}
	cfg := gitcfg.NewConfig()
	data, err := ioutil.ReadFile(gitConf)
	if err != nil {
		return fmt.Errorf("Failed to load %s due to %s", gitConf, err)
	}

	err = cfg.Unmarshal(data)
	if err != nil {
		return fmt.Errorf("Failed to unmarshal %s due to %s", gitConf, err)
	}
	remotes := cfg.Remotes
	if len(remotes) == 0 {
		return nil
	}
	url := getRemoteUrl(cfg, "upstream")
	if url == "" {
		url = getRemoteUrl(cfg, "origin")
		if url == "" {
			url, err = o.pickRemoteURL(cfg)
			if err != nil {
				return err
			}
		}
	}
	if url != "" {
		o.RepoURL = url
	}
	return nil
}

func (o *ImportOptions) DoImport() error {
	url := o.RepoURL
	if url == "" {
		return fmt.Errorf("No Git repository URL found!")
	}
	out := o.Out
	jenk := o.Jenkins
	gitInfo, err := gits.ParseGitURL(url)
	if err != nil {
		return fmt.Errorf("Failed to parse git URL %s due to: %s", url, err)
	}
	org := gitInfo.Organisation
	folder, err := jenk.GetJob(org)
	if err != nil {
		// could not find folder so lets try create it
		jobUrl := util.UrlJoin(jenk.BaseURL(), jenk.GetJobURLPath(org))
		folderXml := jenkins.CreateFolderXml(jobUrl, org)
		//fmt.Fprintf(out, "XML: %s\n", folderXml)
		err = jenk.CreateJobWithXML(folderXml, org)
		if err != nil {
			return fmt.Errorf("Failed to create the %s folder in jenkins: %s", org, err)
		}
		//fmt.Fprintf(out, "Created Jenkins folder: %s\n", org)
	} else {
		c := folder.Class
		if c != "com.cloudbees.hudson.plugins.folder.Folder" {
			fmt.Fprintf(out, "Warning the folder %s is of class %s", org, c)
		}
	}
	projectXml := jenkins.CreateMultiBranchProjectXml(gitInfo, o.Credentials)
	jobName := gitInfo.Name
	job, err := jenk.GetJobByPath(org, jobName)
	if err == nil {
		return fmt.Errorf("Job already exists in Jenkins at " + job.Url)
	}
	//fmt.Fprintf(out, "Creating MultiBranchProject %s from XML: %s\n", jobName, projectXml)
	err = jenk.CreateFolderJobWithXML(projectXml, org, jobName)
	if err != nil {
		return fmt.Errorf("Failed to create MultiBranchProject job %s in folder %s due to: %s", jobName, org, err)
	}
	job, err = jenk.GetJobByPath(org, jobName)
	if err != nil {
		return fmt.Errorf("Failed to find the MultiBranchProject job %s in folder %s due to: %s", jobName, org, err)
	}
	fmt.Fprintf(out, "Created Project: %s\n", job.Url)
	params := neturl.Values{}
	err = jenk.Build(job, params)
	if err != nil {
		return fmt.Errorf("Failed to trigger job %s due to %s", job.Url, err)
	}
	return nil
}

func (o *ImportOptions) pickRemoteURL(config *gitcfg.Config) (string, error) {
	urls := []string{}
	if config.Remotes != nil {
		for _, r := range config.Remotes {
			if r.URLs != nil {
				for _, u := range r.URLs {
					urls = append(urls, u)
				}
			}
		}
	}
	if len(urls) == 1 {
		return urls[0], nil
	}
	url := ""
	if len(urls) > 1 {
		prompt := &survey.Select{
			Message: "Choose a remote git URL:",
			Options: urls,
		}
		err := survey.AskOne(prompt, &url, nil)
		if err != nil {
			return "", err
		}
	}
	return url, nil
}

func firstRemoteUrl(remote *gitcfg.RemoteConfig) string {
	if remote != nil {
		urls := remote.URLs
		if urls != nil && len(urls) > 0 {
			return urls[0]
		}
	}
	return ""
}
func getRemoteUrl(config *gitcfg.Config, name string) string {
	if config.Remotes != nil {
		return firstRemoteUrl(config.Remotes[name])
	}
	return ""
}