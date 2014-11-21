package stage0

//
// Rocket is a reference implementation of the app container specification.
//
// Execution on Rocket is divided into a number of stages, and the `rkt`
// binary implements the first stage (stage 0), which consists of the
// following tasks:
// - Generating a Container Unique ID (UID)
// - Generating a Container Runtime Manifest
// - Creating a filesystem for the container
// - Setting up stage 1 and stage 2 directories in the filesystem
// - Copying the stage1 binary into the container filesystem
// - Fetching the specified application TAFs
// - Unpacking the TAFs and copying the RAF for each app into the stage2
//
// Given a run command such as:
//	rkt run --volume bind:/opt/tenant1/database \
//		example.com/data-downloader-1.0.0 \
//		example.com/ourapp-1.0.0 \
//		example.com/logbackup-1.0.0
//
// the container manifest generated will be compliant with the ACE spec.
//

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"syscall"

	// WARNING: here be dragons
	// TODO(jonboulle): vendor this once the schema is stable
	"code.google.com/p/go-uuid/uuid"
	"github.com/containers/standard/schema"
	"github.com/containers/standard/schema/types"
	"github.com/containers/standard/taf"
	"github.com/coreos-inc/rkt/rkt"
)

type Config struct {
	RktDir       string
	Stage1Init   string
	Stage1Rootfs string
	Debug        bool
	Images       []string
	Volumes      map[string]string
}

func init() {
	log.SetOutput(ioutil.Discard)
}

func Run(cfg Config) {
	if cfg.Debug {
		log.SetOutput(os.Stderr)
	}
	if cfg.RktDir == "" {
		log.Printf("rktDir unset - using temporary directory")
		var err error
		cfg.RktDir, err = ioutil.TempDir("", "rkt")
		if err != nil {
			log.Fatalf("error creating temporary directory: %v", err)
		}
	}

	// - Generating the Container Unique ID (UID)
	cuuid, err := types.NewUUID(uuid.New())
	if err != nil {
		log.Fatalf("error creating UID: %v", err)
	}

	// Create a directory for this container
	dir := filepath.Join(cfg.RktDir, cuuid.String())

	// - Creating a filesystem for the container
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Fatalf("error creating directory: %v", err)
	}

	log.Printf("Writing stage1 rootfs")
	fh, err := os.Open(cfg.Stage1Rootfs)
	if err != nil {
		log.Fatalf("error opening stage1 rootfs: %v", err)
	}
	gz, err := gzip.NewReader(fh)
	if err != nil {
		log.Fatalf("error reading tarball: %v", err)
	}
	rfs := rkt.Stage1RootfsPath(dir)
	if err = os.MkdirAll(rfs, 0776); err != nil {
		log.Fatalf("error creating stage1 rootfs directory: %v", err)
	}
	if err := taf.ExtractTar(tar.NewReader(gz), rfs); err != nil {
		log.Fatalf("error extracting TAF: %v", err)
	}

	log.Printf("Writing stage1 init")
	in, err := os.Open(cfg.Stage1Init)
	if err != nil {
		log.Fatalf("error loading stage1 binary: %v", err)
	}
	fn := rkt.Stage1InitPath(dir)
	out, err := os.OpenFile(fn, os.O_CREATE|os.O_WRONLY, 0555)
	if err != nil {
		log.Fatalf("error opening stage1 init for writing: %v", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		log.Fatalf("error writing stage1 init: %v", err)
	}
	if err := out.Close(); err != nil {
		log.Fatalf("error closing stage1 init: %v", err)
	}

	log.Printf("Wrote filesystem to %s\n", dir)

	// - Generating a Container Runtime Manifest
	cm := schema.ContainerRuntimeManifest{
		ACKind: "ContainerRuntimeManifest",
		UUID:   *cuuid,
		Apps:   map[types.ACLabel]schema.App{},
	}

	v, err := types.NewSemVer(rkt.Version)
	if err != nil {
		log.Fatalf("error creating version: %v", err)
	}
	cm.ACVersion = *v

	// - Fetching the specified application TAFs
	//   (for now, we just assume they are local and named by their hash, and unencrypted)
	// - Unpacking the TAFs and copying the RAF for each app into the stage2

	// TODO(jonboulle): clarify imagehash<->appname. Right now we have to
	// unpack the entire TAF to access the manifest which contains the appname.

	for _, img := range cfg.Images {
		h, err := types.NewHash(img)
		if err != nil {
			log.Fatalf("bad hash given: %v", err)
		}

		log.Println("Loading app image", img)
		fh, err := os.Open(img)
		if err != nil {
			log.Fatalf("error opening app: %v", err)
		}
		gz, err := gzip.NewReader(fh)
		if err != nil {
			log.Fatalf("error reading tarball: %v", err)
		}

		// Sanity check: provided image name matches image ID
		b, err := ioutil.ReadAll(gz)
		if err != nil {
			log.Fatalf("error reading tarball: %v", err)
		}
		sum := sha256.Sum256(b)
		if id := fmt.Sprintf("%x", sum); id != h.Val {
			log.Fatalf("app manifest hash does not match expected")
		}

		ad := rkt.AppImagePath(dir, img)
		err = os.MkdirAll(ad, 0776)
		if err != nil {
			log.Fatalf("error creating app directory: %v", err)
		}
		if err := taf.ExtractTar(tar.NewReader(bytes.NewReader(b)), ad); err != nil {
			log.Fatalf("error extracting TAF: %v", err)
		}

		err = os.MkdirAll(filepath.Join(ad, "rootfs/tmp"), 0777)
		if err != nil {
			log.Fatalf("error creating tmp directory: %v", err)
		}

		mpath := rkt.AppManifestPath(dir, img)
		f, err := os.Open(mpath)
		if err != nil {
			log.Fatalf("error opening app manifest: %v", err)
		}
		b, err = ioutil.ReadAll(f)
		if err != nil {
			log.Fatalf("error reading app manifest: %v", err)
		}
		var am schema.AppManifest
		if err := json.Unmarshal(b, &am); err != nil {
			log.Fatalf("error unmarshaling app manifest: %v", err)
		}

		if _, ok := cm.Apps[am.Name]; ok {
			log.Fatalf("got multiple apps by name %s", am.Name)
		}

		a := schema.App{
			ImageID:     *h,
			Isolators:   am.Isolators,
			Annotations: am.Annotations,
		}

		cm.Apps[am.Name] = a
	}

	var sVols []types.Volume
	for key, path := range cfg.Volumes {
		v := types.Volume{
			Kind:     "host",
			Source:   path,
			ReadOnly: true,
			Fulfills: []types.ACLabel{
				types.ACLabel(key),
			},
		}
		sVols = append(sVols, v)
	}
	cm.Volumes = sVols

	cdoc, err := json.Marshal(cm)
	if err != nil {
		log.Fatalf("error marshalling container manifest: %v", err)
	}

	log.Printf("Writing container manifest")
	fn = rkt.ContainerManifestPath(dir)
	if err := ioutil.WriteFile(fn, cdoc, 0700); err != nil {
		log.Fatalf("error writing container manifest: %v", err)
	}

	log.Printf("Pivoting to filesystem")
	if err := os.Chdir(dir); err != nil {
		log.Fatalf("failed changing to dir: %v", err)
	}

	log.Printf("Execing stage1/init")
	init := "stage1/init"
	args := []string{init}
	if cfg.Debug {
		args = append(args, "debug")
	}
	if err := syscall.Exec(init, args, os.Environ()); err != nil {
		log.Fatalf("error execing init: %v", err)
	}
}
