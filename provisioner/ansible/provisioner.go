package ansible

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/mitchellh/packer/common"
	"github.com/mitchellh/packer/helper/config"
	"github.com/mitchellh/packer/packer"
	"github.com/mitchellh/packer/template/interpolate"
)

type Config struct {
	common.PackerConfig `mapstructure:",squash"`
	ctx                 interpolate.Context

	// The command to run ansible
	Command string

	// Extra options to pass to the ansible command
	ExtraArguments []string `mapstructure:"extra_arguments"`

	// The main playbook file to execute.
	PlaybookFile         string `mapstructure:"playbook_file"`
	LocalPort            string `mapstructure:"local_port"`
	SSHHostKeyFile       string `mapstructure:"ssh_host_key_file"`
	SSHAuthorizedKeyFile string `mapstructure:"ssh_authorized_key_file"`
	SFTPCmd              string `mapstructure:"sftp_command"`
	inventoryFile        string
}

type Provisioner struct {
	config  Config
	adapter *adapter
	done    chan struct{}
}

func (p *Provisioner) Prepare(raws ...interface{}) error {
	p.done = make(chan struct{})

	err := config.Decode(&p.config, &config.DecodeOpts{
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{},
		},
	}, raws...)
	if err != nil {
		return err
	}

	// Defaults
	if p.config.Command == "" {
		p.config.Command = "ansible-playbook"
	}

	var errs *packer.MultiError
	err = validateFileConfig(p.config.PlaybookFile, "playbook_file", true)
	if err != nil {
		errs = packer.MultiErrorAppend(errs, err)
	}

	// Check that the authorized key file exists ( this should really be called the public key )
	// Check for either file ( if you specify either file you must specify both files )
	if len(p.config.SSHAuthorizedKeyFile) > 0 || len(p.config.SSHHostKeyFile) > 0 {
		err = validateFileConfig(p.config.SSHAuthorizedKeyFile, "ssh_authorized_key_file", true)
		if err != nil {
			errs = packer.MultiErrorAppend(errs, err)
		}

		err = validateFileConfig(p.config.SSHHostKeyFile, "ssh_host_key_file", true)
		if err != nil {
			log.Println(p.config.SSHHostKeyFile, "does not exist")
			errs = packer.MultiErrorAppend(errs, err)
		}
	}

	if len(p.config.LocalPort) > 0 {
		if _, err := strconv.ParseUint(p.config.LocalPort, 10, 16); err != nil {
			errs = packer.MultiErrorAppend(errs, fmt.Errorf("local_port: %s must be a valid port", p.config.LocalPort))
		}
	}

	if errs != nil && len(errs.Errors) > 0 {
		return errs
	}
	return nil
}

type Keys struct {
	public   ssh.PublicKey
	private  ssh.Signer
	filename string
}

func (p *Provisioner) Provision(ui packer.Ui, comm packer.Communicator) error {
	ui.Say("Provisioning with Ansible...")

	keyFactory := func(pubKeyFile string, privKeyFile string) (*Keys, error) {
		var public ssh.PublicKey
		var private ssh.Signer

		if len(pubKeyFile) > 0 || len(privKeyFile) > 0 {
			pubKeyBytes, err := ioutil.ReadFile(pubKeyFile)
			if err != nil {
				return nil, errors.New("Failed to read public key")
			}
			public, _, _, _, err := ssh.ParseAuthorizedKey(pubKeyBytes)
			if err != nil {
				return nil, errors.New("Failed to parse authorized key")
			}

			privateBytes, err := ioutil.ReadFile(privKeyFile)
			if err != nil {
				return nil, errors.New("Failed to load private host key")
			}

			private, err := ssh.ParsePrivateKey(privateBytes)
			if err != nil {
				return nil, errors.New("Failed to parse private host key")
			}
			return &Keys{public, private, privKeyFile}, nil
		} else {
			key, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				return nil, errors.New("Failed to generate key pair")
			}
			public, err = ssh.NewPublicKey(key.Public())
			if err != nil {
				return nil, errors.New("Failed to extract public key from generated key pair")
			}
			private, err = ssh.NewSignerFromKey(key)
			if err != nil {
				return nil, errors.New("Failed to extract private key from generated key pair")
			}

			// To support Ansible calling back to us we need to write
			// this file down
			privateKeyDer := x509.MarshalPKCS1PrivateKey(key)
			privateKeyBlock := pem.Block{
				Type:    "RSA PRIVATE KEY",
				Headers: nil,
				Bytes:   privateKeyDer,
			}
			tf, err := ioutil.TempFile("", "ansible-key")
			if err != nil {
				return nil, errors.New("failed to create temp file for generated key")
			}
			_, err = tf.Write(pem.EncodeToMemory(&privateKeyBlock))
			if err != nil {
				return nil, errors.New("failed to write private key to temp file")
			}

			err = tf.Close()
			if err != nil {
				return nil, errors.New("failed to close private key temp file")
			}

			return &Keys{public, private, tf.Name()}, nil
		}
	}

	k, err := keyFactory(p.config.SSHAuthorizedKeyFile, p.config.SSHHostKeyFile)
	if err != nil {
		return err
	}

	keyChecker := ssh.CertChecker{
		UserKeyFallback: func(conn ssh.ConnMetadata, pubKey ssh.PublicKey) (*ssh.Permissions, error) {
			if user := conn.User(); user != "packer-ansible" {
				ui.Say(fmt.Sprintf("%s is not a valid user", user))
				return nil, errors.New("authentication failed")
			}

			if !bytes.Equal(k.public.Marshal(), pubKey.Marshal()) {
				ui.Say("unauthorized key")
				return nil, errors.New("authentication failed")
			}

			return nil, nil
		},
	}
	config := &ssh.ServerConfig{
		AuthLogCallback: func(conn ssh.ConnMetadata, method string, err error) {
			ui.Say(fmt.Sprintf("authentication attempt from %s to %s as %s using %s", conn.RemoteAddr(), conn.LocalAddr(), conn.User(), method))
		},
		PublicKeyCallback: keyChecker.Authenticate,
		//NoClientAuth:      true,
	}

	config.AddHostKey(k.private)

	localListener, err := func() (net.Listener, error) {
		port, _ := strconv.ParseUint(p.config.LocalPort, 10, 16)
		if port == 0 {
			port = 2200
		}
		for i := 0; i < 10; i++ {
			port++
			l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err == nil {
				p.config.LocalPort = strconv.FormatUint(port, 10)
				return l, nil
			}

			ui.Say(err.Error())
		}
		return nil, errors.New("Error setting up SSH proxy connection")
	}()

	if err != nil {
		return err
	}

	p.adapter = newAdapter(p.done, localListener, config, p.config.SFTPCmd, ui, comm)

	defer func() {
		ui.Say("shutting down the SSH proxy")
		close(p.done)
		p.adapter.Shutdown()
	}()

	go p.adapter.Serve()

	if len(p.config.inventoryFile) == 0 {
		tf, err := ioutil.TempFile("", "packer-provisioner-ansible")
		if err != nil {
			return fmt.Errorf("Error preparing inventory file: %s", err)
		}
		defer os.Remove(tf.Name())
		inv := fmt.Sprintf("default ansible_ssh_host=127.0.0.1 ansible_ssh_user=packer-ansible ansible_ssh_port=%s", p.config.LocalPort)
		_, err = tf.Write([]byte(inv))
		if err != nil {
			tf.Close()
			return fmt.Errorf("Error preparing inventory file: %s", err)
		}
		tf.Close()
		p.config.inventoryFile = tf.Name()
		defer func() {
			p.config.inventoryFile = ""
		}()
	}

	if err := p.executeAnsible(ui, comm, k.filename); err != nil {
		return fmt.Errorf("Error executing Ansible: %s", err)
	}

	return nil

}

func (p *Provisioner) Cancel() {
	if p.done != nil {
		close(p.done)
	}
	if p.adapter != nil {
		p.adapter.Shutdown()
	}
	os.Exit(0)
}

func (p *Provisioner) executeAnsible(ui packer.Ui, comm packer.Communicator, authToken string) error {
	playbook, _ := filepath.Abs(p.config.PlaybookFile)
	inventory := p.config.inventoryFile

	args := []string{playbook, "-i", inventory, "--private-key", authToken}
	args = append(args, p.config.ExtraArguments...)

	cmd := exec.Command(p.config.Command, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}

	wg := sync.WaitGroup{}
	repeat := func(r io.ReadCloser) {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			ui.Message(scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			ui.Error(err.Error())
		}
		wg.Done()
	}
	wg.Add(2)
	go repeat(stdout)
	go repeat(stderr)

	ui.Say(fmt.Sprintf("Executing Ansible: %s", strings.Join(cmd.Args, " ")))
	cmd.Start()
	wg.Wait()
	err = cmd.Wait()
	if err != nil {
		return fmt.Errorf("Non-zero exit status: %s", err)
	}

	return nil
}

func validateFileConfig(name string, config string, req bool) error {
	if req {
		if name == "" {
			return fmt.Errorf("%s must be specified.", config)
		}
	}
	info, err := os.Stat(name)
	if err != nil {
		return fmt.Errorf("%s: %s is invalid: %s", config, name, err)
	} else if info.IsDir() {
		return fmt.Errorf("%s: %s must point to a file", config, name)
	}
	return nil
}
