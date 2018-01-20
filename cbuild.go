package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/aws/aws-sdk-go/service/ssm"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"strings"
)

type Builder struct {
	sess         *session.Session
	docker       *client.Client
	registryAuth string
	tmpdir       string
	registryID   string
	imagename    string
}

func NewBuilder() *Builder {
	var err error
	b := new(Builder)

	b.tmpdir, err = ioutil.TempDir("", uuid.New().String())
	if err != nil {
		log.Fatal(err)
	}

	b.sess = session.Must(session.NewSession())

	b.sess.Config.Region = aws.String("eu-west-1")

	b.docker, err = client.NewEnvClient()
	if err != nil {
		panic(err)
	}

	return b
}

func (b *Builder) SetRegion(region string) {
	b.sess.Config.Region = aws.String(region)
}

func (b *Builder) Cleanup() {
	os.RemoveAll(b.tmpdir)
}

func (b *Builder) registrylogin(registryID string) (string, error) {
	ecr_client := ecr.New(b.sess)

	input := ecr.GetAuthorizationTokenInput{}
	if registryID != "" {
		input.RegistryIds = []*string{&registryID}
	} //else default regitstry of the account is assumed
	auth, err := ecr_client.GetAuthorizationToken(&input)
	if err != nil {
		log.Printf("RegistryLogin: Error: %s\n", err)
		return "", err
	}
	// b.token = *auth.AuthorizationData[0].AuthorizationToken
	// // fmt.Printf("token: %s\n", ac.token)
	// b.exp = auth.AuthorizationData[0].ExpiresAt
	b.registryID = strings.TrimPrefix(*auth.AuthorizationData[0].ProxyEndpoint, "https://")
	log.Printf("RegistryLogin: Got authentication token for registry: %s\n", b.registryID)

	data, err := base64.StdEncoding.DecodeString(*auth.AuthorizationData[0].AuthorizationToken)
	if err != nil {
		return "", err
	}
	credentials := strings.Split(string(data), ":")
	// fmt.Printf("%#v\n", strings.Split(string(data), ":"))

	authConfig := types.AuthConfig{
		Username:      credentials[0],
		Password:      credentials[1],
		ServerAddress: *auth.AuthorizationData[0].ProxyEndpoint, //"https://248243587295.dkr.ecr.eu-west-1.amazonaws.com",
	}

	authConfigJSON, _ := json.Marshal(authConfig)
	b.registryAuth = base64.URLEncoding.EncodeToString(authConfigJSON)

	return b.registryAuth, nil
}

//TODO: allow to specify the platform for the pushed image
func (b *Builder) push() error {
	//check if repository exists
	ecr_client := ecr.New(b.sess)

	_, err := ecr_client.DescribeRepositories(&ecr.DescribeRepositoriesInput{
		RepositoryNames: []*string{&b.imagename},
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() == ecr.ErrCodeRepositoryNotFoundException { //if the repo isn't found create a new one
				_, err := ecr_client.CreateRepository(&ecr.CreateRepositoryInput{
					RepositoryName: &b.imagename,
				})
				if err != nil {
					log.Printf("Push: Error: %s\n", err)
					return err
				}
			} else { //fail on all other errors
				log.Printf("Push: Error: %s\n", err)
				return err
			}
		}
	}

	options := types.ImagePushOptions{
		All:          true,
		RegistryAuth: b.registryAuth,
		//PrivilegeFunc: ac.RequestPrivilege,
		//  Platform:      "",
	}
	// fmt.Printf("token: %s\n", options.RegistryAuth)
	// registry := strings.TrimPrefix(b.registryURL, "https://")
	r, err := b.docker.ImagePush(context.Background(), b.registryID+"/"+b.imagename, options) //"248243587295.dkr.ecr.eu-west-1.amazonaws.com/pyspark", options)
	if err != nil {
		panic(err)
	}

	d := json.NewDecoder(r)
	for {
		var v map[string]interface{}
		if err = d.Decode(&v); err != nil { //no need to handle errors other than EOF differently
			break
		}

		if status, exists := v["status"]; exists {
			if status != "Pushing" { //omit messages regarding pushing progress from output
				if id, exists := v["id"]; exists {
					log.Printf("Push: %s id:%s", status, id)
				} else {
					log.Printf("Push: %s", status)
				}
			}
		}
		if aux, exists := v["aux"]; exists {
			m := aux.(map[string]interface{})
			log.Printf("Push: Tag: %s, Digest: %s, Size: %f\n", m["Tag"].(string), m["Digest"].(string), m["Size"].(float64))
		}

	}

	return nil
}

func (b *Builder) buildFromS3(bucket string, filename string) {

}

//TODO: improve logging of ImageBuild to also handle "aux" JSON objects in the command's output
//subdirectory within the repo.
//For details see https://docs.docker.com/engine/reference/commandline/build/#git-repositories
func (b *Builder) buildFromGithub(repo string, authkey string, imagename string) error {
	var credentials, repoURL string

	if strings.Contains(repo, ".git") != true {
		return errors.New("invalid repository name (missing .git ending)")
	}

	if imagename == "" { //if no imagename defined use the repo name
		if strings.Contains(repo, "#") {
			components := strings.Split(repo, "#")
			frgament := components[1]
			if strings.Contains(frgament, ":") { // if fragment contains subfolder
				components = strings.Split(frgament, ":")
				imagename = components[1] //imagename is the subfolder name
			} else { //fragment doesn't contain subfolder
				imagename = components[1] //imagename is the branch name
				//repo = strings.TrimSuffix(components[0], ".git")

			}
		} else { //repo URL doesn't contain fragment i.e. no "#" in the URL
			repo_URI := strings.TrimSuffix(repo, ".git")
			components := strings.Split(repo_URI, "/")
			imagename = components[len(components)-1] //imagename is the last path component of the URL
		}
	}

	if authkey != "" {
		//get username and password from Systems MAnager parameter store
		var decrypt bool = true
		ssm_client := ssm.New(b.sess)

		key, err := ssm_client.GetParameter(&ssm.GetParameterInput{
			Name:           &authkey,
			WithDecryption: &decrypt,
		})
		if err != nil {
			log.Printf("Build: Error: %s\n", err)
			return err
		}
		byte_str, err := base64.URLEncoding.DecodeString(*key.Parameter.Value)
		credentials = string(byte_str)
		// fmt.Printf("%s\n", credentials)

		repoURL = "https://" + credentials + "@" + repo
	} else { // assume this is a public URL that doesn't require authentication
		repoURL = "https://" + repo
	}

	_, err := b.registrylogin("")
	if err != nil {
		return err
	}

	tag := b.registryID + "/" + imagename //strings.TrimPrefix(b.registryURL, "https://") + "/" + imagename

	options := types.ImageBuildOptions{
		Tags:          []string{tag},
		RemoteContext: repoURL,
		ForceRemove:   true,
	}

	log.Printf("Building %s...\n", tag)
	response, err := b.docker.ImageBuild(context.Background(), nil, options)
	if err != nil {
		log.Printf("Push: Error: %s\n", err)
		return err
	}

	d := json.NewDecoder(response.Body)
	for {
		var v map[string]interface{}
		if err = d.Decode(&v); err != nil { //no need to handle errors other than EOF differently
			break
		}
		if status, exists := v["stream"]; exists {
			log.Printf("Build: %s", status)
		}
		if aux, exists := v["aux"]; exists {
			m := aux.(map[string]interface{})
			log.Printf("Build: ID: %s", m["ID"])
		}
	}

	b.imagename = imagename

	b.push()

	return nil
}

func (b *Builder) downloadBuildCtx(bucket string, filename string) {

	s := session.Must(session.NewSession())
	// s.Config.Region = aws.String("eu-central-1")

	c := s3.New(s)

	f, err := os.Create(filename)
	if err != nil {
		panic(err) //return fmt.Errorf("failed to create file %q, %v", filename, err)
	}
	defer f.Close()

	// buf := make([]byte, 10000000, 10000000)
	// bufWriter := aws.NewWriteAtBuffer(buf)

	downloader := s3manager.NewDownloaderWithClient(c)

	length, err := downloader.Download(f, &s3.GetObjectInput{
		Bucket: aws.String("apapa3"),
		Key:    aws.String("authz-build.tgz"),
	})
	if err != nil {
		panic(err)
	}

	cmd_str := fmt.Sprintf("tar -xfz %s -C %s", filename, b.tmpdir)
	out, err := exec.Command(cmd_str).Output()
	if err != nil {
		panic(err)
	}
	fmt.Printf("%d, %v\n", length, out)
}

var cname, repo, region, credentials_ssm_key string

func init() {
	flag.StringVar(&cname, "name", "", "the name of the container image (default value is the last part of the repo name)")
	flag.StringVar(&repo, "r", "", "the name of the GitHub repository containing the container build files")
	flag.StringVar(&credentials_ssm_key, "c", "", "the name of an AWS systems manager parameter containing credentials for the GitHub repository. Not required for public repositories")
	flag.StringVar(&region, "region", "", "The AWS region")

}

func main() {
	flag.Parse()

	b := NewBuilder()

	if repo == "" {
		log.Fatal("GitHub repository name is required")
	}

	if region != "" {
		b.SetRegion(region)
	}

	err := b.buildFromGithub(repo, credentials_ssm_key, cname)
	if err != nil {
		panic(err)
	}
}
