package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// https://stackoverflow.com/a/28323276
type arrayFlags []string

func (i *arrayFlags) String() string {
	return strings.Join(*i, " ")
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
	builtBy = "unknown"

	mergeFileFlags arrayFlags

	// flags
	flagDecrypt      = flag.Bool("decrypt", false, "Decrypt single resource")
	flagGenIdentity  = flag.String("gen-identity", "", "Generate a new identity and add it to your strongbox identity file")
	flagGenKey       = flag.String("gen-key", "", "Generate a new key and add it to your strongbox keyring")
	flagGitConfig    = flag.Bool("git-config", false, "Configure git for strongbox use")
	flagIdentityFile = flag.String("identity-file", "", "strongbox identity file, if not set default '$HOME/.strongbox_identity' will be used")
	flagKey          = flag.String("key", "", "Private key to use to decrypt")
	flagKeyRing      = flag.String("keyring", "", "strongbox keyring file path, if not set default '$HOME/.strongbox_keyring' will be used")
	flagRecursive    = flag.Bool("recursive", false, "Recursively decrypt all files under given folder, must be used with -decrypt flag")

	flagClean  = flag.String("clean", "", "intended to be called internally by git")
	flagSmudge = flag.String("smudge", "", "intended to be called internally by git")
	flagDiff   = flag.String("diff", "", "intended to be called internally by git")

	flagVersion = flag.Bool("version", false, "Strongbox version")
)

func usage() {
	fmt.Fprintf(os.Stderr, "Usage:\n\n")
	fmt.Fprintf(os.Stderr, "\tstrongbox -git-config\n")
	fmt.Fprintf(os.Stderr, "\tstrongbox [-identity-file PATH] -gen-identity IDENTITY_NAME\n")
	fmt.Fprintf(os.Stderr, "\tstrongbox [-keyring KEYRING_FILEPATH] -gen-key KEY_NAME\n")
	fmt.Fprintf(os.Stderr, "\tstrongbox [-keyring KEYRING_FILEPATH] -decrypt -recursive [-key KEY] [PATH]\n")
	fmt.Fprintf(os.Stderr, "\tstrongbox [-keyring KEYRING_FILEPATH] -decrypt -key KEY [PATH]\n")
	fmt.Fprintf(os.Stderr, "\tstrongbox -version\n")
	fmt.Fprintf(os.Stderr, "\n(age) if -identity-file flag is not set, default '$HOME/.strongbox_identity' will be used\n")
	fmt.Fprintf(os.Stderr, "(siv) if -keyring flag is not set default file '$HOME/.strongbox_keyring' or '$STRONGBOX_HOME/.strongbox_keyring' will be used as keyring\n")
	os.Exit(2)
}

func main() {
	log.SetPrefix("strongbox: ")
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	flag.Var(&mergeFileFlags, "merge-file", "intended to be called internally by git")

	flag.Usage = usage
	flag.Parse()

	if *flagVersion || (flag.NArg() == 1 && flag.Arg(0) == "version") {
		fmt.Printf("version=%s commit=%s date=%s builtBy=%s\n", version, commit, date, builtBy)
		return
	}

	if *flagGitConfig {
		gitConfig()
		return
	}

	if *flagDiff != "" {
		diff(*flagDiff)
		return
	}

	// Set up keyring file name
	home := deriveHome()
	kr = &fileKeyRing{fileName: filepath.Join(home, ".strongbox_keyring")}

	if *flagIdentityFile != "" {
		identityFilename = *flagIdentityFile
	} else {
		identityFilename = filepath.Join(home, defaultIdentityFilename)
	}

	// if keyring flag is set replace default keyRing
	if *flagKeyRing != "" {
		kr = &fileKeyRing{fileName: *flagKeyRing}
		// verify keyring is valid
		if err := kr.Load(); err != nil {
			log.Fatalf("unable to load keyring file:%s err:%s", *flagKeyRing, err)
		}
	}

	if *flagGenKey != "" {
		genKey(*flagGenKey)
		return
	}

	if *flagDecrypt {
		// handle recursive
		if *flagRecursive {
			var err error

			target := flag.Arg(0)
			if target == "" {
				target, err = os.Getwd()
				if err != nil {
					log.Fatalf("target path not provided and unable to get cwd err:%s", err)
				}
			}
			// for recursive decryption 'key' flag is optional but if provided
			// it should be valid and all encrypted file will be decrypted using it
			dk, err := decode([]byte(*flagKey))
			if err != nil && *flagKey != "" {
				log.Fatalf("Unable to decode given private key %v", err)
			}

			if err = recursiveDecrypt(target, dk); err != nil {
				log.Fatalln(err)
			}
			return
		}

		if *flagKey == "" {
			log.Fatalf("Must provide a `-key` when using -decrypt")
		}
		decryptCLI()
		return
	}

	if *flagRecursive {
		log.Println("-recursive flag is only supported with -decrypt")
		usage()
	}

	if *flagGenIdentity != "" {
		ageGenIdentity(*flagGenIdentity)
		return
	}

	if *flagClean != "" {
		clean(os.Stdin, os.Stdout, *flagClean)
		return
	}
	if *flagSmudge != "" {
		smudge(os.Stdin, os.Stdout, *flagSmudge)
		return
	}
	if len(mergeFileFlags) > 0 {
		if len(mergeFileFlags) != 8 {
			log.Fatalf("expected 8 -merge-file arguments, got %d: %v", len(mergeFileFlags), mergeFileFlags)
		}
		os.Exit(mergeFile())
	}
}

func deriveHome() string {
	// try explicitly set STRONGBOX_HOME
	if home := os.Getenv("STRONGBOX_HOME"); home != "" {
		return home
	}
	// try HOME env var
	if home := os.Getenv("HOME"); home != "" {
		return home
	}
	// Try user.Current which works in most cases, but may not work with CGO disabled.
	u, err := user.Current()
	if err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	log.Fatal("Could not find $STRONGBOX_HOME, $HOME or call os/user.Current(). Please set $STRONGBOX_HOME, $HOME or recompile with CGO enabled")
	// not reached
	return ""
}

func decryptCLI() {
	var fn string
	if flag.Arg(0) == "" {
		// no file passed, try to read stdin
		fn = "/dev/stdin"
	} else {
		fn = flag.Arg(0)
	}
	fb, err := os.ReadFile(fn)
	if err != nil {
		log.Fatalf("Unable to read file to decrypt %v", err)
	}
	dk, err := decode([]byte(*flagKey))
	if err != nil {
		log.Fatalf("Unable to decode private key %v", err)
	}
	out, err := decrypt(fb, dk)
	if err != nil {
		log.Fatalf("Unable to decrypt %v", err)
	}
	fmt.Printf("%s", out)
}

func gitConfig() {
	args := [][]string{
		{"config", "--global", "--replace-all", "filter.strongbox.clean", "strongbox -clean %f"},
		{"config", "--global", "--replace-all", "filter.strongbox.smudge", "strongbox -smudge %f"},
		{"config", "--global", "--replace-all", "filter.strongbox.required", "true"},

		{"config", "--global", "--replace-all", "diff.strongbox.textconv", "strongbox -diff"},
		{"config", "--global", "--replace-all", "merge.strongbox.driver", "strongbox -merge-file %O -merge-file %A -merge-file %B -merge-file %L -merge-file %P -merge-file %S -merge-file %X -merge-file %Y"},
	}
	for _, command := range args {
		cmd := exec.Command("git", command...)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Fatal(string(out))
		}
	}
	log.Println("git global configuration updated successfully")
}

func diff(filename string) {
	f, err := os.Open(filename)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err = f.Close(); err != nil {
			log.Fatal(err)
		}
	}()
	_, err = io.Copy(os.Stdout, f)
	if err != nil {
		log.Fatal(err)
	}
}

func clean(r io.Reader, w io.Writer, filename string) {
	// Read the file, fail on error
	in, err := io.ReadAll(r)
	if err != nil {
		log.Fatal(err)
	}
	// Check the file is plaintext, if its an encrypted strongbox or age file, copy as is, and exit 0
	if bytes.HasPrefix(in, prefix) || strings.HasPrefix(string(in), armor.Header) {
		_, err = io.Copy(w, bytes.NewReader(in))
		if err != nil {
			log.Fatal(err)
		}
		return
	}
	// File is plaintext and needs to be encrypted, get the recipient or a
	// key, fail on error
	recipient, key, err := findRecipients(filename)
	if err != nil {
		log.Fatal(err)
	}

	// found recipient file and plaintext differs from HEAD
	if recipient != nil {
		ageEncrypt(w, recipient, in, filename)
	}
	if key != nil {
		// encrypt the file, fail on error
		out, err := encrypt(in, key)
		if err != nil {
			log.Fatal(err)
		}
		// write out encrypted file, fail on error
		_, err = io.Copy(w, bytes.NewReader(out))
		if err != nil {
			log.Fatal(err)
		}
	}
}

// Called by git on `git checkout`
func smudge(r io.Reader, w io.Writer, filename string) {
	in, err := io.ReadAll(r)
	if err != nil {
		log.Fatal(err)
	}

	if strings.HasPrefix(string(in), armor.Header) {
		ageDecrypt(w, in)
		return
	}
	if bytes.HasPrefix(in, prefix) {
		key, err := keyLoader(filename)
		if err != nil {
			// don't log error if its keyNotFound
			switch err {
			case errKeyNotFound:
			default:
				log.Println(err)
			}
			// Couldn't load the key, just copy as is and return
			if _, err = io.Copy(w, bytes.NewReader(in)); err != nil {
				log.Println(err)
			}
			return
		}

		out, err := decrypt(in, key)
		if err != nil {
			log.Println(err)
			out = in
		}
		if _, err := io.Copy(w, bytes.NewReader(out)); err != nil {
			log.Println(err)
		}
		return
	}

	// file is a non-siv and non-age file, copy as is and exit
	_, err = io.Copy(w, bytes.NewReader(in))
	if err != nil {
		log.Fatal(err)
	}
}

func mergeFile() int {
	// https://git-scm.com/docs/gitattributes#_defining_a_custom_merge_driver
	//
	// The merge driver is expected to leave the result of the merge in the file
	// named with %A by overwriting it, and exit with zero status if it managed to
	// merge them cleanly, or non-zero if there were conflicts. When the driver
	// crashes it is expected to exit with non-zero status that are higher than 128,
	// and in such a case, the merge results in a failure (which is different
	// from producing a conflict). hence exit code -1 is used here on failure
	base := mergeFileFlags[0]       // %O
	current := mergeFileFlags[1]    // %A
	other := mergeFileFlags[2]      // %B
	markerSize := mergeFileFlags[3] // %L
	_ = mergeFileFlags[4]           // %P
	label1 := mergeFileFlags[5]     // %S
	label2 := mergeFileFlags[6]     // %X
	label3 := mergeFileFlags[7]     // %Y

	tempBase, err := smudgeToFile(base) // Smudge base
	if err != nil {
		log.Printf("%s", err)
		return -1
	}
	defer os.Remove(tempBase)

	tempCurrent, err := smudgeToFile(current) // Smudge current
	if err != nil {
		log.Printf("%s", err)
		return -1
	}
	defer os.Remove(tempCurrent)

	tempOther, err := smudgeToFile(other) // Smudge other
	if err != nil {
		log.Printf("%s", err)
		return -1
	}
	defer os.Remove(tempOther)

	var stdOut bytes.Buffer
	var errOut bytes.Buffer
	// Run git merge-file
	cmd := exec.Command("git", "merge-file",
		"--marker-size="+markerSize,
		"--stdout",
		"-L", label1,
		"-L", label2,
		"-L", label3,
		tempCurrent,
		tempBase,
		tempOther)
	cmd.Stdout = &stdOut
	cmd.Stderr = &errOut

	// The exit value of `git merge-file` is negative on error, and the number of
	// conflicts otherwise (truncated to 127 if there are more than that many conflicts).
	// If the merge was clean, the exit value is 0.
	mergeErr := cmd.Run()

	// write merged value if produced
	if stdOut.Len() > 0 {
		if err := os.WriteFile(current, stdOut.Bytes(), 0644); err != nil {
			log.Printf("failed to write merged file: %s", err)
			return -1
		}
	}

	// match exit code of `git merge-file` command
	if mergeErr != nil {
		var execError *exec.ExitError
		if errors.As(mergeErr, &execError) {
			fmt.Println(errOut.String())
			return execError.ExitCode()
		}
		log.Printf("git merge-file failed: %s  %s", errOut.String(), mergeErr)
		return -1
	}
	return 0
}

func smudgeToFile(filename string) (string, error) {
	// Open the input file
	file, err := os.Open(filename)
	if err != nil {
		return "", fmt.Errorf("failed to open file %s: %w", filename, err)
	}
	defer file.Close()

	// Create a buffer to hold the processed output
	var buf strings.Builder
	smudge(file, &buf, filename)

	// Write the buffer content to a temporary file
	return createTempFile(buf.String()), nil
}

func createTempFile(content string) string {
	// Create a temporary file
	tmpFile, err := os.CreateTemp("", "merge-file-*.tmp")
	if err != nil {
		log.Fatalf("failed to create temporary file: %v", err)
	}
	defer tmpFile.Close()

	// Write the content to the file
	if _, err := tmpFile.WriteString(content); err != nil {
		log.Fatalf("failed to write to temporary file: %v", err)
	}

	return tmpFile.Name() // Return the file path
}

// Finds closest age recipient or siv keyid
func findRecipients(filename string) ([]age.Recipient, []byte, error) {
	path := filepath.Dir(filename)
	for {
		if fi, err := os.Stat(path); err == nil && fi.IsDir() {
			ageRecipientFilename := filepath.Join(path, recipientFilename)
			// If we found `.strongbox_recipient` - parse it and return
			if keyFile, err := os.Stat(ageRecipientFilename); err == nil && !keyFile.IsDir() {
				recipients, err := ageFileToRecipient(ageRecipientFilename)
				return recipients, nil, err
			}
			// If we found `strongbox-keyid` - get the corresponding key and return it
			keyFilename := filepath.Join(path, ".strongbox-keyid")
			if keyFile, err := os.Stat(keyFilename); err == nil && !keyFile.IsDir() {
				key, err := sivFileToKey(keyFilename)
				return nil, key, err
			}
		}
		if path == "." {
			return nil, nil, fmt.Errorf("failed to find recipient or keyid for file %s", filename)
		}
		path = filepath.Dir(path)
	}
}
