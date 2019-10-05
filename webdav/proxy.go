// Helper functions for interfacing with AIStore proxy
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */

package main

import (
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/NVIDIA/aistore/api"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/tutils"
)

type proxyServer struct {
	url string
}

// createBucket creates a new bucket
func (p *proxyServer) createBucket(bucket string) error {
	baseParams := tutils.BaseAPIParams(p.url)
	return api.CreateBucket(baseParams, bucket)
}

func (p *proxyServer) deleteBucket(bucket string) error {
	baseParams := tutils.BaseAPIParams(p.url)
	return api.DestroyBucket(baseParams, bucket)
}

func (p *proxyServer) doesBucketExist(bucket string) bool {
	// note: webdav works with ais bucket only (at least for now)
	// _, err := api.HeadBucket(p.url, bucket)
	// return err == nil
	baseParams := tutils.BaseAPIParams(p.url)
	bns, err := api.GetBucketNames(baseParams, cmn.AIS /* local */)
	if err != nil {
		return false
	}

	for _, b := range bns.AIS {
		if b == bucket {
			return true
		}
	}

	return false
}

// listBuckets returns a slice of names of all buckets
func (p *proxyServer) listBuckets(provider string) ([]string, error) {
	if provider == cmn.Cloud || provider == "" {
		return nil, nil
	}
	baseParams := tutils.BaseAPIParams(p.url)
	bns, err := api.GetBucketNames(baseParams, provider)
	if err != nil {
		return nil, err
	}

	return bns.AIS, nil
}

// doesObjectExists checks whether a resource exists by querying AIStore.
func (p *proxyServer) doesObjectExist(bucket, prefix string) (bool, *fileInfo, error) {
	entries, err := p.listObjectsDetails(bucket, "", prefix, 1)
	if err != nil {
		return false, nil, err
	}

	if len(entries) == 0 {
		return false, nil, nil
	}

	if entries[0].Name == prefix {
		t, _ := time.Parse(time.RFC822, entries[0].Atime)
		return true, &fileInfo{size: entries[0].Size, modTime: t.UTC()}, nil
	}

	if strings.HasPrefix(entries[0].Name, prefix+"/") {
		return true, &fileInfo{mode: os.ModeDir}, nil
	}

	return false, nil, nil
}

// putObject creates a new file reader and uses it to make a proxy put call to save a new
// object with xxHash enabled into a bucket.
func (p *proxyServer) putObject(localPath, bucket, provider, prefix string) error {
	r, err := tutils.NewFileReaderFromFile(localPath, true /* xxhash */)
	if err != nil {
		return err
	}
	baseParams := tutils.BaseAPIParams(p.url)
	putArgs := api.PutObjectArgs{
		BaseParams: baseParams,
		Bucket:     bucket,
		Provider:   provider,
		Object:     prefix,
		Hash:       r.XXHash(),
		Reader:     r,
	}
	return api.PutObject(putArgs)
}

// getObject asks proxy to return an object and saves it into the io.Writer (for example, a local file).
func (p *proxyServer) getObject(bucket, provider, prefix string, w io.Writer) error {
	query := url.Values{}
	query.Add(cmn.URLParamProvider, provider)
	baseParams := tutils.BaseAPIParams(p.url)
	options := api.GetObjectInput{Writer: w, Query: query}
	_, err := api.GetObjectWithValidation(baseParams, bucket, prefix, options)
	return err
}

func (p *proxyServer) deleteObject(bucket, provider, prefix string) error {
	return tutils.Del(p.url, bucket, prefix, provider, nil /* wg */, nil /* errCh */, true /* silent */)
}

// listObjectsDetails returns details of all objects that matches the prefix in a bucket
func (p *proxyServer) listObjectsDetails(bucket, provider, prefix string, limit int) ([]*cmn.BucketEntry, error) {
	msg := &cmn.SelectMsg{
		Prefix: prefix,
		Props:  "size, ctime",
	}
	query := url.Values{}
	query.Add(cmn.URLParamProvider, provider)
	baseParams := tutils.BaseAPIParams(p.url)
	bl, err := api.ListBucket(baseParams, bucket, msg, limit, query)
	if err != nil {
		return nil, err
	}

	return bl.Entries, err
}

// listObjectsNames returns names of all objects that matches the prefix in a bucket
func (p *proxyServer) listObjectsNames(bucket, provider, prefix string) ([]string, error) {
	return tutils.ListObjects(p.url, bucket, provider, prefix, 0)
}

// deleteObjects deletes all objects in the list of names from a bucket
func (p *proxyServer) deleteObjects(bucket, provider string, names []string) error {
	return api.DeleteList(tutils.BaseAPIParams(p.url), bucket, provider, names, true /* wait */, 0 /* deadline*/)
}
