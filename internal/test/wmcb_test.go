package test

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"testing"
	"time"

	"github.com/masterzen/winrm"
	"github.com/openshift/windows-machine-config-operator/tools/windows-node-installer/pkg/cloudprovider"
	"github.com/openshift/windows-machine-config-operator/tools/windows-node-installer/pkg/types"
	"github.com/pkg/sftp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

const (
	// user for the Windows node created.
	// TODO: remove this hardcoding to any user.
	user = "Administrator"
	// remotePowerShellCmdPrefix holds the powershell prefix that needs to be prefixed to every command run on the
	// remote powershell session opened
	remotePowerShellCmdPrefix = "powershell.exe -NonInteractive -ExecutionPolicy Bypass "
	// winrm port to be used
	winRMPort = 5986
)

// testFramework holds the info to run the test suite
type testFramework struct {
	// credentials to access the Windows VM created
	credentials *types.Credentials
	// winrmClient to access the Windows VM created
	winrmClient *winrm.Client
	// fileNames contains a list of files to be transferred to the Windows VM
	fileNames []string
	// remoteDir is the directory to which files will be transferred to, on the Windows VM
	remoteDir string
	// sshClient contains the ssh client information to access the Windows VM via ssh
	sshClient *ssh.Client
	// cloudProvider holds the information related to cloud provider
	cloudProvider cloudprovider.Cloud
}

// framework holds the instantiation of test suite being executed. As of now, temp dir is hardcoded.
// TODO: Create a temporary remote directory on the Windows node
var framework = &testFramework{remoteDir: "C:\\Temp"}

// binaryToBeTransferred holds the binary that needs to be transferred to the Windows VM
// TODO: Make this an array later with a comma separated values for more binaries to be transferred
var binaryToBeTransferred = flag.String("binaryToBeTransferred", "",
	"Absolute path of the binary to be transferred")

// setup sets up the initial test cluster for the Windows
// TODO: move this to return error and do assertions there
func (f *testFramework) setup() {
	if err := framework.createWindowsVM(); err != nil {
		log.Fatalf("failed to create Windows VM: %v", err)
	}
	// TODO: Add some options to skip certain parts of the test
	if err := framework.setupWinRMClient(); err != nil {
		log.Fatalf("failed to setup winRM client for the Windows VM: %v", err)
	}
	// Wait for some time before starting configuring of ssh server. This is to let sshd service be available
	// in the list of services
	// TODO: Parse the output of the `Get-Service sshd, ssh-agent` on the Windows node to check if the windows nodes
	// has those services present
	time.Sleep(time.Minute)
	if err := framework.configureOpenSSHServer(); err != nil {
		log.Fatalf("failed to configure OpenSSHServer on the Windows VM: %v", err)
	}
	if err := framework.createRemoteDir(); err != nil {
		log.Fatalf("failed to create remote dir with error: %v", err)
	}
	if err := framework.getSSHClient(); err != nil {
		log.Fatalf("failed to get ssh client for the Windows VM created: %v", err)
	}
}

// createWindowsVM spins up the Windows VM in the given cloud provider and gives us the credentials to access the
// windows VM created
func (f *testFramework) createWindowsVM() error {
	kubeconfig := os.Getenv("KUBECONFIG")
	awsCredentials := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	artifactDir := os.Getenv("ARTIFACT_DIR")
	privateKeyPath := os.Getenv("KUBE_SSH_KEY_PATH")
	// Use Windows 2019 server image with containers in us-east1 zone for CI testing.
	// TODO: Move to environment variable that can be fetched from the cloud provider
	imageID := "ami-0b8d82dea356226d3"
	instanceType := "m4.large"
	sshKey := "libra"
	cloud, err := cloudprovider.CloudProviderFactory(kubeconfig, awsCredentials, "default", artifactDir,
		imageID, instanceType, sshKey, privateKeyPath)
	if err != nil {
		return fmt.Errorf("error instantiating cloud provider %v", err)
	}
	f.cloudProvider = cloud
	credentials, err := cloud.CreateWindowsVM()
	if err != nil {
		return fmt.Errorf("error creating Windows VM: %v", err)
	}
	f.credentials = credentials
	return nil
}

// setupWinRMClient sets up the winrm client to be used while accessing Windows node
func (f *testFramework) setupWinRMClient() error {
	host := f.credentials.GetIPAddress()
	password := f.credentials.GetPassword()

	endpoint := winrm.NewEndpoint(host, winRMPort, true, true, nil, nil, nil, 0)
	winrmClient, err := winrm.NewClient(endpoint, user, password)
	if err != nil {
		return fmt.Errorf("failed to set up winrm client with error: %v", err)
	}
	f.winrmClient = winrmClient
	return nil
}

// configureOpenSSHServer configures the OpenSSH server using WinRM client installed on the Windows VM.
// The OpenSSH server is installed as part of WNI tool's CreateVM method.
func (f *testFramework) configureOpenSSHServer() error {
	// This dependency is needed for the subsequent module installation we're doing. This version of NuGet
	// needed for OpenSSH server 0.0.1
	installDependentPackages := "Install-PackageProvider -Name NuGet -MinimumVersion 2.8.5.201 -Force"
	if _, err := f.winrmClient.Run(remotePowerShellCmdPrefix+installDependentPackages,
		os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("failed to install dependent packages for OpenSSH server with error %v", err)
	}
	// Configure OpenSSH for all users.
	// TODO: Limit this to Administrator.
	if _, err := f.winrmClient.Run(remotePowerShellCmdPrefix+"Install-Module -Force OpenSSHUtils -Scope AllUsers",
		os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("failed to configure OpenSSHUtils for all users with error %v", err)
	}
	// Setup ssh-agent Windows Service.
	if _, err := f.winrmClient.Run(remotePowerShellCmdPrefix+"Set-Service -Name ssh-agent -StartupType ‘Automatic’",
		os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("failed to set up ssh-agent Windows Service with err %v", err)
	}
	// Setup sshd Windows service
	if _, err := f.winrmClient.Run(remotePowerShellCmdPrefix+"Set-Service -Name sshd -StartupType ‘Automatic’",
		os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("failed to set up sshd Windows Service with err %v", err)
	}
	if _, err := f.winrmClient.Run(remotePowerShellCmdPrefix+"Start-Service ssh-agent",
		os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("start ssh-agent with error %v", err)
	}
	if _, err := f.winrmClient.Run(remotePowerShellCmdPrefix+"Start-Service sshd",
		os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("failed to start sshd with error %v", err)
	}
	return nil
}

// createRemoteDir creates a directory on the Windows VM to which file can be transferred
func (f *testFramework) createRemoteDir() error {
	// Create a directory on the Windows node where the file has to be transferred
	if _, err := framework.winrmClient.Run(remotePowerShellCmdPrefix+"mkdir"+" "+framework.remoteDir,
		os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("failed to created a temporary dir on the remote Windows node with %v", err)
	}
	return nil
}

// getSSHClient gets the ssh client associated with Windows VM created
func (f *testFramework) getSSHClient() error {
	config := &ssh.ClientConfig{
		User:            "Administrator",
		Auth:            []ssh.AuthMethod{ssh.Password(framework.credentials.GetPassword())},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	sshClient, err := ssh.Dial("tcp", framework.credentials.GetIPAddress()+":22", config)
	if err != nil {
		return fmt.Errorf("failed to dial to ssh server: %s", err)
	}
	framework.sshClient = sshClient
	return nil
}

// TestWMCBUnit runs the unit tests for WMCB
func TestWMCBUnit(t *testing.T) {
	// Transfer the binary to the windows using scp
	defer framework.sshClient.Close()
	sftp, err := sftp.NewClient(framework.sshClient)
	require.NoError(t, err, "sftp client initialization failed")
	defer sftp.Close()
	f, err := os.Open(*binaryToBeTransferred)
	require.NoError(t, err, "error opening binary file to be transferred")
	dstFile, err := sftp.Create(framework.remoteDir + "\\" + "wmcb_unit_test.exe")
	require.NoError(t, err, "error opening binary file to be transferred")
	_, err = io.Copy(dstFile, f)
	require.NoError(t, err, "error copying binary to the Windows VM")

	// Forcefully close it so that we can execute the binary later
	dstFile.Close()

	stdout := os.Stdout
	r, w, err := os.Pipe()
	assert.NoError(t, err, "error opening pipe to read stdout")
	os.Stdout = w

	// Remotely execute the test binary.
	_, err = framework.winrmClient.Run(remotePowerShellCmdPrefix+framework.remoteDir+"\\"+
		"wmcb_unit_test.exe --test.v",
		os.Stdout, os.Stderr)
	assert.NoError(t, err, "error while executing the test binary remotely")
	w.Close()
	out, err := ioutil.ReadAll(r)
	assert.NoError(t, err, "error reading stdout from the remote Windows VM")
	os.Stdout = stdout
	assert.NotContains(t, string(out), "FAIL")
}

// tearDown tears down the set up done for test suite
func (f *testFramework) tearDown() {
	if err := f.cloudProvider.DestroyWindowsVMs(); err != nil {
		log.Fatalf("failed tearing down the Windows VM with error: %v", err)
	}
}

func TestMain(m *testing.M) {
	framework.setup()
	testStatus := m.Run()
	// TODO: Add one more check to remove lingering cloud resources
	framework.tearDown()
	os.Exit(testStatus)

}