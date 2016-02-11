package fly_test

import (
	"os"
	"os/exec"

	"github.com/concourse/testflight/helpers"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"testing"
)

var (
	flyBin  string
	tmpHome string
)

var atcURL = helpers.AtcURL()
var targetedConcourse = "testflight"

var _ = SynchronizedBeforeSuite(func() []byte {
	Eventually(helpers.ErrorPolling(atcURL)).ShouldNot(HaveOccurred())

	data, err := helpers.FirstNodeFlySetup(atcURL, targetedConcourse)
	Expect(err).NotTo(HaveOccurred())

	return data
}, func(data []byte) {
	var err error
	flyBin, tmpHome, err = helpers.AllNodeFlySetup(data)
	Expect(err).NotTo(HaveOccurred())

	//For tests that require at least one build to have run
	executeSimpleTask()
})

var _ = SynchronizedAfterSuite(func() {
}, func() {
	os.RemoveAll(tmpHome)
})

func TestFly(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Authentication Fly Suite")
}

func executeSimpleTask() {
	fly := exec.Command(flyBin, "-t", targetedConcourse, "execute", "-c", "../fixtures/simple-task.yml")
	session := helpers.StartFly(fly)

	<-session.Exited
	Expect(session.ExitCode()).To(Equal(0))
}
