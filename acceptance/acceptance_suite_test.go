package acceptance

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
	"github.com/pivotal-cf-experimental/pivnet-resource/concourse"
	"github.com/pivotal-cf-experimental/pivnet-resource/logger"
	"github.com/pivotal-cf-experimental/pivnet-resource/pivnet"
	"github.com/pivotal-cf-experimental/pivnet-resource/sanitizer"

	"testing"
)

var (
	inPath    string
	checkPath string
	outPath   string

	endpoint string

	productSlug        string
	pivnetAPIToken     string
	awsAccessKeyID     string
	awsSecretAccessKey string
	pivnetRegion       string
	pivnetBucketName   string
	s3FilepathPrefix   string

	pivnetClient pivnet.Client
)

func TestAcceptance(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Acceptance Suite")
}

var _ = BeforeSuite(func() {
	var err error
	By("Getting product slug from environment variables")
	productSlug = os.Getenv("PRODUCT_SLUG")
	Expect(productSlug).NotTo(BeEmpty(), "$PRODUCT_SLUG must be provided")

	By("Getting API token from environment variables")
	pivnetAPIToken = os.Getenv("API_TOKEN")
	Expect(pivnetAPIToken).NotTo(BeEmpty(), "$API_TOKEN must be provided")

	By("Getting aws access key id from environment variables")
	awsAccessKeyID = os.Getenv("AWS_ACCESS_KEY_ID")
	Expect(awsAccessKeyID).NotTo(BeEmpty(), "$AWS_ACCESS_KEY_ID must be provided")

	By("Getting aws secret access key from environment variables")
	awsSecretAccessKey = os.Getenv("AWS_SECRET_ACCESS_KEY")
	Expect(awsSecretAccessKey).NotTo(BeEmpty(), "$AWS_SECRET_ACCESS_KEY must be provided")

	By("Getting pivnet region from environment variables")
	pivnetRegion = os.Getenv("PIVNET_S3_REGION")
	Expect(pivnetRegion).NotTo(BeEmpty(), "$PIVNET_S3_REGION must be provided")

	By("Getting pivnet bucket name from environment variables")
	pivnetBucketName = os.Getenv("PIVNET_BUCKET_NAME")
	Expect(pivnetBucketName).NotTo(BeEmpty(), "$PIVNET_BUCKET_NAME must be provided")

	By("Getting s3 filepath prefix from environment variables")
	s3FilepathPrefix = os.Getenv("S3_FILEPATH_PREFIX")
	Expect(s3FilepathPrefix).NotTo(BeEmpty(), "$S3_FILEPATH_PREFIX must be provided")

	By("Getting endpoint from environment variables")
	endpoint = os.Getenv("PIVNET_ENDPOINT")
	Expect(endpoint).NotTo(BeEmpty(), "$PIVNET_ENDPOINT must be provided")

	By("Compiling check binary")
	checkPath, err = gexec.Build("github.com/pivotal-cf-experimental/pivnet-resource/cmd/check", "-race")
	Expect(err).NotTo(HaveOccurred())

	By("Compiling out binary")
	outPath, err = gexec.Build("github.com/pivotal-cf-experimental/pivnet-resource/cmd/out", "-race")
	Expect(err).NotTo(HaveOccurred())

	By("Compiling in binary")
	inPath, err = gexec.Build("github.com/pivotal-cf-experimental/pivnet-resource/cmd/in", "-race")
	Expect(err).NotTo(HaveOccurred())

	By("Copying s3-out to compilation location")
	originalS3OutPath := os.Getenv("S3_OUT_LOCATION")
	Expect(originalS3OutPath).ToNot(BeEmpty(), "$S3_OUT_LOCATION must be provided")
	_, err = os.Stat(originalS3OutPath)
	Expect(err).NotTo(HaveOccurred())
	s3OutPath := filepath.Join(path.Dir(outPath), "s3-out")
	copyFileContents(originalS3OutPath, s3OutPath)
	Expect(err).NotTo(HaveOccurred())

	By("Ensuring copy of s3-out is executable")
	err = os.Chmod(s3OutPath, os.ModePerm)
	Expect(err).NotTo(HaveOccurred())

	By("Sanitizing acceptance test output")
	sanitized := map[string]string{
		pivnetAPIToken:     "***sanitized-api-token***",
		awsAccessKeyID:     "***sanitized-aws-access-key-id***",
		awsSecretAccessKey: "***sanitized-aws-secret-access-key***",
	}
	sanitizer := sanitizer.NewSanitizer(sanitized, GinkgoWriter)
	GinkgoWriter = sanitizer

	By("Creating pivnet client (for out-of-band operations)")
	testLogger := logger.NewLogger(GinkgoWriter)

	clientConfig := pivnet.NewClientConfig{
		Endpoint:  endpoint,
		Token:     pivnetAPIToken,
		UserAgent: "pivnet-resource/integration-test",
	}
	pivnetClient = pivnet.NewClient(clientConfig, testLogger)
})

var _ = AfterSuite(func() {
	gexec.CleanupBuildArtifacts()
})

func getReleases(productSlug string) []pivnet.Release {
	productURL := fmt.Sprintf(
		"%s/api/v2/products/%s/releases",
		endpoint,
		productSlug,
	)

	req, err := http.NewRequest("GET", productURL, nil)
	Expect(err).NotTo(HaveOccurred())

	req.Header.Add("Authorization", fmt.Sprintf("Token %s", pivnetAPIToken))

	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))

	response := pivnet.Response{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	Expect(err).NotTo(HaveOccurred())

	return response.Releases
}

func getProductVersions(productSlug string) []string {
	var versions []string
	for _, release := range getReleases(productSlug) {
		versions = append(versions, string(release.Version))
	}

	return versions
}

func getPivnetRelease(productSlug, productVersion string) pivnet.Release {
	for _, release := range getReleases(productSlug) {
		if release.Version == productVersion {
			return release
		}
	}
	Fail(fmt.Sprintf(
		"Could not find release for productSlug: %s and productVersion: %s",
		productSlug,
		productVersion,
	))
	// We won't get here
	return pivnet.Release{}
}

func deletePivnetRelease(productSlug, productVersion string) {
	pivnetRelease := getPivnetRelease(productSlug, productVersion)
	releaseID := pivnetRelease.ID
	Expect(releaseID).NotTo(Equal(0))

	productURL := fmt.Sprintf(
		"%s/api/v2/products/%s/releases/%d",
		endpoint,
		productSlug,
		releaseID,
	)

	req, err := http.NewRequest("DELETE", productURL, nil)
	Expect(err).NotTo(HaveOccurred())

	req.Header.Add("Authorization", fmt.Sprintf("Token %s", pivnetAPIToken))

	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusNoContent))
}

func getProductFiles(productSlug string) []pivnet.ProductFile {
	productURL := fmt.Sprintf(
		"%s/api/v2/products/%s/product_files",
		endpoint,
		productSlug,
	)

	req, err := http.NewRequest("GET", productURL, nil)
	Expect(err).NotTo(HaveOccurred())

	req.Header.Add("Authorization", fmt.Sprintf("Token %s", pivnetAPIToken))

	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))

	response := pivnet.ProductFiles{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	Expect(err).NotTo(HaveOccurred())

	return response.ProductFiles
}

func getUserGroups(productSlug string, releaseID int) []pivnet.UserGroup {
	userGroupsURL := fmt.Sprintf(
		"%s/api/v2/products/%s/releases/%d/user_groups",
		endpoint,
		productSlug,
		releaseID,
	)

	req, err := http.NewRequest("GET", userGroupsURL, nil)
	Expect(err).NotTo(HaveOccurred())

	req.Header.Add("Authorization", fmt.Sprintf("Token %s", pivnetAPIToken))

	resp, err := http.DefaultClient.Do(req)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.StatusCode).To(Equal(http.StatusOK))

	response := pivnet.UserGroups{}
	err = json.NewDecoder(resp.Body).Decode(&response)
	Expect(err).NotTo(HaveOccurred())

	return response.UserGroups
}

// copyFileContents copies the contents of the file named src to the file named
// by dst. The file will be created if it does not already exist. If the
// destination file exists, all it's contents will be replaced by the contents
// of the source file.
// See http://stackoverflow.com/questions/21060945/simple-way-to-copy-a-file-in-golang
func copyFileContents(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return
	}
	defer func() {
		cerr := out.Close()
		if err == nil {
			err = cerr
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return
	}
	err = out.Sync()
	return
}

func run(command *exec.Cmd, stdinContents []byte) *gexec.Session {
	fmt.Fprintf(GinkgoWriter, "input: %s\n", stdinContents)

	stdin, err := command.StdinPipe()
	Expect(err).ShouldNot(HaveOccurred())

	session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
	Expect(err).NotTo(HaveOccurred())

	_, err = io.WriteString(stdin, string(stdinContents))
	Expect(err).ShouldNot(HaveOccurred())

	err = stdin.Close()
	Expect(err).ShouldNot(HaveOccurred())

	return session
}

func metadataValueForKey(metadata []concourse.Metadata, name string) (string, error) {
	for _, i := range metadata {
		if i.Name == name {
			return i.Value, nil
		}
	}
	return "", fmt.Errorf("name not found: %s", name)
}
