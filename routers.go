/*
 * Minio Cloud Storage, (C) 2015, 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"fmt"
	"net"
	"net/http"

	"github.com/elazarl/go-bindata-assetfs"
	"github.com/gorilla/handlers"
	router "github.com/gorilla/mux"
	jsonrpc "github.com/gorilla/rpc/v2"
	"github.com/gorilla/rpc/v2/json2"
	"github.com/minio/minio-go"
	"github.com/minio/minio/pkg/fs"
	"github.com/minio/minio/pkg/probe"
	"github.com/minio/minio/pkg/s3/signature4"
	"github.com/minio/miniobrowser"
)

// storageAPI container for S3 compatible API.
type storageAPI struct {
	// Filesystem instance.
	Filesystem fs.Filesystem
	// Signature instance.
	Signature *signature4.Sign
}

// webAPI container for Web API.
type webAPI struct {
	// FSPath filesystem path.
	FSPath string
	// Minio client instance.
	Client *minio.Client

	// private params.
	apiAddress string // api destination address.
	// credential kept to be used internally.
	accessKeyID     string
	secretAccessKey string
}

// indexHandler - Handler to serve index.html
type indexHandler struct {
	handler http.Handler
}

func (h indexHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.URL.Path = privateBucket + "/"
	h.handler.ServeHTTP(w, r)
}

const assetPrefix = "production"

func assetFS() *assetfs.AssetFS {
	return &assetfs.AssetFS{
		Asset:     miniobrowser.Asset,
		AssetDir:  miniobrowser.AssetDir,
		AssetInfo: miniobrowser.AssetInfo,
		Prefix:    assetPrefix,
	}
}

// specialAssets are files which are unique files not embedded inside index_bundle.js.
const specialAssets = "loader.css|logo.svg|firefox.png|safari.png|chrome.png|favicon.ico"

// registerAPIHandlers - register all the handlers to their respective paths
func registerAPIHandlers(mux *router.Router, a storageAPI, w *webAPI) {
	// Minio rpc router
	minio := mux.NewRoute().PathPrefix(privateBucket).Subrouter()

	// Initialize json rpc handlers.
	rpc := jsonrpc.NewServer()
	codec := json2.NewCodec()
	rpc.RegisterCodec(codec, "application/json")
	rpc.RegisterCodec(codec, "application/json; charset=UTF-8")
	rpc.RegisterService(w, "Web")

	// RPC handler at URI - /minio/rpc
	minio.Path("/rpc").Handler(rpc)
	// Serve all assets.
	minio.Path(fmt.Sprintf("/{assets:[^/]+.js|%s}", specialAssets)).Handler(handlers.CompressHandler(http.StripPrefix(privateBucket, http.FileServer(assetFS()))))
	// Serve index.html for rest of the requests
	minio.Path("/{index:.*}").Handler(indexHandler{http.StripPrefix(privateBucket, http.FileServer(assetFS()))})

	// API Router
	api := mux.NewRoute().PathPrefix("/").Subrouter()

	// Bucket router
	bucket := api.PathPrefix("/{bucket}").Subrouter()

	/// Object operations

	// HeadObject
	bucket.Methods("HEAD").Path("/{object:.+}").HandlerFunc(a.HeadObjectHandler)
	// PutObjectPart
	bucket.Methods("PUT").Path("/{object:.+}").HandlerFunc(a.PutObjectPartHandler).Queries("partNumber", "{partNumber:[0-9]+}", "uploadId", "{uploadId:.*}")
	// ListObjectPxarts
	bucket.Methods("GET").Path("/{object:.+}").HandlerFunc(a.ListObjectPartsHandler).Queries("uploadId", "{uploadId:.*}")
	// CompleteMultipartUpload
	bucket.Methods("POST").Path("/{object:.+}").HandlerFunc(a.CompleteMultipartUploadHandler).Queries("uploadId", "{uploadId:.*}")
	// NewMultipartUpload
	bucket.Methods("POST").Path("/{object:.+}").HandlerFunc(a.NewMultipartUploadHandler).Queries("uploads", "")
	// AbortMultipartUpload
	bucket.Methods("DELETE").Path("/{object:.+}").HandlerFunc(a.AbortMultipartUploadHandler).Queries("uploadId", "{uploadId:.*}")
	// GetObject
	bucket.Methods("GET").Path("/{object:.+}").HandlerFunc(a.GetObjectHandler)
	// CopyObject
	bucket.Methods("PUT").Path("/{object:.+}").HeadersRegexp("X-Amz-Copy-Source", ".*?(\\/).*?").HandlerFunc(a.CopyObjectHandler)
	// PutObject
	bucket.Methods("PUT").Path("/{object:.+}").HandlerFunc(a.PutObjectHandler)
	// DeleteObject
	bucket.Methods("DELETE").Path("/{object:.+}").HandlerFunc(a.DeleteObjectHandler)

	/// Bucket operations

	// GetBucketLocation
	bucket.Methods("GET").HandlerFunc(a.GetBucketLocationHandler).Queries("location", "")
	// GetBucketPolicy
	bucket.Methods("GET").HandlerFunc(a.GetBucketPolicyHandler).Queries("policy", "")
	// ListMultipartUploads
	bucket.Methods("GET").HandlerFunc(a.ListMultipartUploadsHandler).Queries("uploads", "")
	// ListObjects
	bucket.Methods("GET").HandlerFunc(a.ListObjectsHandler)
	// PutBucketPolicy
	bucket.Methods("PUT").HandlerFunc(a.PutBucketPolicyHandler).Queries("policy", "")
	// PutBucket
	bucket.Methods("PUT").HandlerFunc(a.PutBucketHandler)
	// HeadBucket
	bucket.Methods("HEAD").HandlerFunc(a.HeadBucketHandler)
	// PostPolicy
	bucket.Methods("POST").HeadersRegexp("Content-Type", "multipart/form-data*").HandlerFunc(a.PostPolicyBucketHandler)
	// DeleteMultipleObjects
	bucket.Methods("POST").HandlerFunc(a.DeleteMultipleObjectsHandler)
	// DeleteBucketPolicy
	bucket.Methods("DELETE").HandlerFunc(a.DeleteBucketPolicyHandler).Queries("policy", "")
	// DeleteBucket
	bucket.Methods("DELETE").HandlerFunc(a.DeleteBucketHandler)

	/// Root operation

	// ListBuckets
	api.Methods("GET").HandlerFunc(a.ListBucketsHandler)
}

// configureServer handler returns final handler for the http server.
func configureServerHandler(filesystem fs.Filesystem) http.Handler {
	// Access credentials.
	cred := serverConfig.GetCredential()

	// Server region.
	region := serverConfig.GetRegion()

	// Server addr.
	addr := serverConfig.GetAddr()

	sign, err := signature4.New(cred.AccessKeyID, cred.SecretAccessKey, region)
	fatalIf(err.Trace(cred.AccessKeyID, cred.SecretAccessKey, region), "Initializing signature version '4' failed.", nil)

	// Initialize API.
	api := storageAPI{
		Filesystem: filesystem,
		Signature:  sign,
	}

	// Split host port.
	host, port, _ := net.SplitHostPort(addr)

	// Default host is 'localhost', if no host present.
	if host == "" {
		host = "localhost"
	}

	// Initialize minio client for AWS Signature Version '4'
	disableSSL := !isSSL() // Insecure true when SSL is false.
	client, e := minio.NewV4(net.JoinHostPort(host, port), cred.AccessKeyID, cred.SecretAccessKey, disableSSL)
	fatalIf(probe.NewError(e), "Unable to initialize minio client", nil)

	// Initialize Web.
	web := &webAPI{
		FSPath:          filesystem.GetRootPath(),
		Client:          client,
		apiAddress:      addr,
		accessKeyID:     cred.AccessKeyID,
		secretAccessKey: cred.SecretAccessKey,
	}

	var handlerFns = []HandlerFunc{
		// Redirect some pre-defined browser request paths to a static
		// location prefix.
		setBrowserRedirectHandler,
		// Validates if incoming request is for restricted buckets.
		setPrivateBucketHandler,
		// Adds cache control for all browser requests.
		setBrowserCacheControlHandler,
		// Validates all incoming requests to have a valid date header.
		setTimeValidityHandler,
		// CORS setting for all browser API requests.
		setCorsHandler,
		// Validates all incoming URL resources, for invalid/unsupported
		// resources client receives a HTTP error.
		setIgnoreResourcesHandler,
		// Auth handler verifies incoming authorization headers and
		// routes them accordingly. Client receives a HTTP error for
		// invalid/unsupported signatures.
		setAuthHandler,
	}

	// Initialize router.
	mux := router.NewRouter()

	// Register all API handlers.
	registerAPIHandlers(mux, api, web)

	// Register rest of the handlers.
	return registerHandlers(mux, handlerFns...)
}
