package main

import (
	"io"
	"io/ioutil"
	"os"
	"fmt"
	"path/filepath"
	"errors"
	b64 "encoding/base64"
	"encoding/json"
	"context"
	"strings"
	"gopkg.in/yaml.v2"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ecr"

	log "github.com/sirupsen/logrus"

	"github.com/docker/docker/builder/dockerignore"
	"github.com/docker/docker/cli/command"
	"github.com/docker/docker/cli/command/image/build"
	"github.com/docker/docker/client"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/progress"


)

// DockerbuildMap 对应于dockerbuild-map.yaml文件结构
type DockerbuildMap struct {
	Apps []App `yaml:"apps" json:"apps"`
}

// App 对应于dockerbuild-map.yaml文件子结构
type App struct {
	Name string `yaml:"name" json:"name"`
	Dockerfile string `yaml:"dockerfile" json:"dockerfile"`
	BuildContext string `yaml:"build_context" json:"build_context"`
	BuildArgs  []BuildArg `yaml:"build_args" json:"build_args"`
}

// BuildArg 对应于dockerbuild-map.yaml文件子结构
type BuildArg struct {
	Name string `yaml:"name" json:"name"`
	Desc string `yaml:"desc" json:"desc"`
	Value string `yaml:"value" json:"value"`
}

// 从dockerbuidMap 结构体中取出特定的App
func (dockerbuildMap *DockerbuildMap) getApp(appName string) (app App, err error) {

	for _, a := range dockerbuildMap.Apps {
		if a.Name == appName {
			return a, nil
		}
	}

	return App{}, errors.New("App not found in dockerbuildMap")
}
// parse BUILD_LIST env into []App
func (dockerbuildMap *DockerbuildMap) parseBuildList() ([]App) {
	appsFromBuildList := []App{}
	appsForBuild := []App{}
	buildList := envMust("BUILD_LIST")
	if err := json.Unmarshal([]byte(buildList), &appsFromBuildList); err != nil {
		log.
			WithField("reporter", `parseBuildList()->json.Unmarshal([]byte(buildList), &apps)`).
			Fatal(err)
	}

	for _, a := range appsFromBuildList {
		app, err := dockerbuildMap.getApp(a.Name)
		if err != nil {
			log.Warnf("%s\n", err)
			continue
		}
		// 没有定义buildContext则跳过
		if app.BuildContext == "" {
			log.Warnf("应用[%s]没有在文件dockerbuild-map.yaml中定义build_context字段，已跳过构建", app.Name)
			continue
		}
		// 检查 buildContext 中是否存在Dockerfile
		_, err = os.Stat(fmt.Sprintf("%s/Dockerfile", app.BuildContext))
		if err != nil {
			log.Warnf("应用[%s]没有在路径%s中提供Dockerfile，已跳过构建", app.Name, app.BuildContext)
			continue
		}
		app.BuildArgs = a.BuildArgs
		appsForBuild = append(appsForBuild, app)
	}
	log.Debugf("以下应用即将被构建： %+v\n", appsForBuild)
	return appsForBuild
}
// Registry ...
type Registry string
// AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY and AWS_DEFAULT_REGION
// 136204750825.dkr.ecr.ap-southeast-1.amazonaws.com
func (r Registry) login() (authStr string) {
	// aws registry handle
	if strings.HasSuffix(string(r), "amazonaws.com") {
		akidEnvName := "AWS_ACCESS_KEY_ID"
		sakEnvName := "AWS_SECRET_ACCESS_KEY"

		akid := os.Getenv(akidEnvName)
		if akid == "" {
			log.Fatalf("检测到环境变量%[1]s为空,请设置环境变量%[1]s后重试\n", akidEnvName)
		}
		sak := os.Getenv(sakEnvName)
		if sak == "" {
			log.Fatalf("检测到环境变量%[1]s为空,请设置环境变量%[1]s后重试\n", sakEnvName)
		}
		_splits := strings.Split(string(r),".")
		if len(_splits) != 6 {
			log.Fatalf("检测到registry: [%s]格式不正确\n", string(r))
		}
		region := _splits[3]
		sess, err := session.NewSession(&aws.Config{
			Region: aws.String(region)},
		)
		if err != nil {
			log.Fatalf("aws会话创建失败: %s\n", err)
		}
		ecrCli := ecr.New(sess)
		input := &ecr.GetAuthorizationTokenInput{}
		result, err := ecrCli.GetAuthorizationToken(input)
		if err != nil {
			if aerr, ok := err.(awserr.Error); ok {
				switch aerr.Code() {
				case ecr.ErrCodeServerException:
					log.Fatalln(ecr.ErrCodeServerException, aerr.Error())
				case ecr.ErrCodeInvalidParameterException:
					log.Fatalln(ecr.ErrCodeInvalidParameterException, aerr.Error())
				default:
					log.Fatalln(aerr.Error())
				}
			} else {
				// Print the error, cast err to awserr.Error to get the Code and
				// Message from an error.
				log.Fatalln(err.Error())
			}
		}
		log.Debug(result)
		token,err := b64.StdEncoding.DecodeString(*result.AuthorizationData[0].AuthorizationToken)
		if err != nil {
			log.Fatalf("token decode failed: %s", err.Error())
		}

		authConfig := types.AuthConfig{
			Username: strings.Split(string(token),":")[0],
			Password: strings.Split(string(token),":")[1],
		}
		encodedJSON, err := json.Marshal(authConfig)
		if err != nil {
			log.Fatal(err)
		}
		authStr = b64.URLEncoding.EncodeToString(encodedJSON)
		
	} else {
		usernameEnvName := "REGISTRY_USERNAME"
		passwordEnvName := "REGISTRY_PASSWORD"
		username := os.Getenv(usernameEnvName)
		if username == "" {
			log.Fatalf("检测到环境变量%[1]s为空,请设置环境变量%[1]s后重试\n", usernameEnvName)
		}
		password := os.Getenv(passwordEnvName)
		if password == "" {
			log.Fatalf("检测到环境变量%[1]s为空,请设置环境变量%[1]s后重试\n", passwordEnvName)
		}
		authConfig := types.AuthConfig{
			Username: username,
			Password: password,
		}
		encodedJSON, err := json.Marshal(authConfig)
		if err != nil {
			log.Fatal(err)
		}
		authStr = b64.URLEncoding.EncodeToString(encodedJSON)
	}
	return authStr
}

// envMust return the env variable which you specified, fatal when not exist.
func envMust(envName string) (value string) {
	le := log.WithField("method", "envMust")
	value = os.Getenv(envName)
	if value == "" {
		le.Fatalf("环境变量[%s]未设定。", envName)
	}
	le.Debug(fmt.Sprintf("环境变量[%s]: %s", envName, value))
	return value
}


func main(){
	switch strings.ToLower(os.Getenv("OPMS_BUILD_HELPER_LOGLEVEL")) {
	case "debug":
		log.SetLevel(log.DebugLevel)
	default:
		log.SetLevel(log.InfoLevel)
	}
	
	registry := Registry(envMust("REGISTRY"))
	namespace := envMust("REGISTRY_NAMESPACE")
	ciCommitSHA := envMust("CI_COMMIT_SHA")
	dockerbuildMapPath := "dockerbuild-map.yaml"
	
	if os.Getenv("DOCKERBUILD_MAP") != "" {
		dockerbuildMapPath = os.Getenv("DOCKERBUILD_MAP")
	}
	if len(ciCommitSHA) < 8 {
		log.Fatal("CI_COMMIT_SHA length is not enough")
	}
	ciCommitSHA = ciCommitSHA[:8]
	
	dockercli, err := client.NewEnvClient()
	if err != nil {
		log.
			WithField("reporter", `client.NewEnvClient()`).
			Fatal(err)
	}

	// 加载dockerbuild-map.yaml到内存
	dockerbuildMapFileContent, err := ioutil.ReadFile(dockerbuildMapPath)
	if err != nil {
		log.
			WithField("reporter", `ioutil.ReadFile(dockerbuildMapPath)`).
			Fatal(err)
	}
	
	dockerbuildMap := DockerbuildMap{}
	err = yaml.Unmarshal(dockerbuildMapFileContent, &dockerbuildMap)
	if err != nil {
		log.
			WithField("reporter", `yaml.Unmarshal(dockerbuildMapFileContent, &dockerbuildMap)`).
			Fatal(err)
	}
	log.Debug("加载dockerbuild-map.yaml到内存")

	
	for _, a := range dockerbuildMap.parseBuildList() {
		imageURL := fmt.Sprintf("%s/%s/%s:%s", registry, namespace, a.Name, ciCommitSHA)
		contextDir := a.BuildContext
		excludes := []string{}
		buildArgs := make(map[string]*string)


		f, err := os.Open(filepath.Join(contextDir, ".dockerignore"))
		if err != nil && !os.IsNotExist(err) {
			log.Fatalf("dockerignore文件读取发生错误: %s\n", err)
		}
		defer f.Close()
		if err == nil {
			excludes, err = dockerignore.ReadAll(f)
			if err != nil {
				log.Fatalf("dockerignore文件读取发生错误: %s\n", err)
			}
		}

		if err := build.ValidateContextDirectory(contextDir, excludes); err != nil {
			log.Fatalf("构建上下文目录验证错误: %s\n", err)
		}

		buildCtx, err := archive.TarWithOptions(contextDir, &archive.TarOptions{
			Compression:     archive.Gzip, // archive.Uncompressed
			ExcludePatterns: excludes,
		})
		if err != nil {
			log.Fatalf("构建上下文目录打包错误: %s\n", err)
		}

		log.Debug(fmt.Sprintf("应用镜像URL: %s", imageURL))
		
		// load buildArgs
		for _, ba := range a.BuildArgs {
			buildArgs[ba.Name] = &ba.Value
		}
		buildOpt := types.ImageBuildOptions{
			Tags: []string{imageURL},
			BuildArgs: buildArgs,
		}
		// Setup an upload progress bar
		progressOutput := streamformatter.NewStreamFormatter().NewProgressOutput(os.Stdout, true)


		var body io.Reader = progress.NewProgressReader(buildCtx, progressOutput, 0, "", "Sending build context to Docker daemon")

		resp, err := dockercli.ImageBuild(context.Background(), body, buildOpt)
		if err != nil {
			log.
				WithField("reporter", `cli.ImageBuild(context.Background(), buildContext, opt)`).
				Fatal(err)
		}
		defer resp.Body.Close()
		outStream := command.NewOutStream(os.Stdout)
		err = jsonmessage.DisplayJSONMessagesToStream(
			resp.Body, outStream, nil,
		)
		if err != nil {
			log.Fatalf("jsonmessage.DisplayJSONMessagesToStream: %s\n", err)
		}
		imagePushResp, err := dockercli.ImagePush(
			context.Background(),
			imageURL,
			types.ImagePushOptions{
				RegistryAuth: registry.login(),
			},
		)
		defer imagePushResp.Close()
		if err != nil {
			log.Fatalf("dockercli.ImagePush: %s\n", err)
		}
		err = jsonmessage.DisplayJSONMessagesToStream(
			imagePushResp, outStream, nil,
		)
		if err != nil {
			log.Fatalf("jsonmessage.DisplayJSONMessagesToStream: %s\n", err)
		}

		// 根据环境变量的传入决定是否删除本地构建的镜像
		if os.Getenv("IMAGE_CLEAN") != "false" {
			dels, err := dockercli.ImageRemove(context.Background(), imageURL, types.ImageRemoveOptions{})
			if err != nil {
				log.Fatalf("dockercli.ImageRemove: %s\n", err)
			} else {
				for _, del := range dels {
					if del.Deleted != "" {
						fmt.Fprintf(os.Stdout, "Deleted: %s\n", del.Deleted)
					} else {
						fmt.Fprintf(os.Stdout, "Untagged: %s\n", del.Untagged)
					}
				}
			}
		}
	}
}
