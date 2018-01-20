package main

import (
	// "bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
		return "", err
	}

	// b.token = *auth.AuthorizationData[0].AuthorizationToken
	// // fmt.Printf("token: %s\n", ac.token)
	// b.exp = auth.AuthorizationData[0].ExpiresAt
	b.registryID = strings.TrimPrefix(*auth.AuthorizationData[0].ProxyEndpoint, "https://")
	fmt.Printf("b.registryID: %s\n", b.registryID)

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

	repositories, err := ecr_client.DescribeRepositories(&ecr.DescribeRepositoriesInput{
		RepositoryNames: []*string{&b.imagename},
	})
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() == ecr.ErrCodeRepositoryNotFoundException { //if the repo isn't found create a new one
				_, err := ecr_client.CreateRepository(&ecr.CreateRepositoryInput{
					RepositoryName: &b.imagename,
				})
				if err != nil {
					return err
				}
			} else { //fail on all other errors
				fmt.Printf("describerepos: %v\n%v\n", repositories, err)
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

	resp, _ := ioutil.ReadAll(r)
	fmt.Println(string(resp))

	return nil
}

func (b *Builder) buildFromS3(bucket string, filename string) {

}

//TODO: allo specification of fragments for the repo to cater for cases where the build context is a
//subdirectory within the repo.
//For details see https://docs.docker.com/engine/reference/commandline/build/#git-repositories
func (b *Builder) buildFromGithub(repo string, authkey string, imagename string) error {
	var credentials, repoURL string

	if imagename == "" {
		return errors.New("imagename can not be empty")
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
			panic(err)
		}
		byte_str, err := base64.URLEncoding.DecodeString(*key.Parameter.Value)
		credentials = string(byte_str)
		// fmt.Printf("%s\n", credentials)

		repoURL = "https://" + credentials + "@" + repo + ".git"
	} else { // assume this is a public URL that doesn't require authentication
		repoURL = "https://" + repo + ".git"
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
		panic(err)
	}

	// type Message struct {
	// 	stream string, `json:"stream"`
	// }

	// var m Message
	d := json.NewDecoder(response.Body)
	for {
		var v map[string]interface{}
		if err = d.Decode(&v); err != nil {
			log.Println(err)
			break
		}
		for k := range v {
			switch k {
			case "stream":
				log.Printf("%s", v["stream"].(string))
			case "aux":
				log.Printf("(aux)%s", v["stream"].(string))
			}
			//fmt.Printf("%#v\n", v)
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

func main() {

	b := NewBuilder()

	err := b.buildFromGithub("github.com/apanto/foo", "/dev/authz/github", "foo")
	if err != nil {
		panic(err)
	}
}
