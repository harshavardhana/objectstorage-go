/*
 * Minio Go Library for Amazon S3 Compatible Cloud Storage (C) 2015, 2016 Minio, Inc.
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

package minio

import (
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"mime"
	"os"
	"path/filepath"
	"sort"
)

// FPutObject - Create an object in a bucket, with contents from file at filePath.
func (c Client) FPutObject(bucketName, objectName, filePath, contentType string) (n int64, err error) {
	// Input validation.
	if err := isValidBucketName(bucketName); err != nil {
		return 0, err
	}
	if err := isValidObjectName(objectName); err != nil {
		return 0, err
	}

	// Open the referenced file.
	fileReader, err := os.Open(filePath)
	// If any error fail quickly here.
	if err != nil {
		return 0, err
	}
	defer fileReader.Close()

	// Save the file stat.
	fileStat, err := fileReader.Stat()
	if err != nil {
		return 0, err
	}

	// Save the file size.
	fileSize := fileStat.Size()

	// Check for largest object size allowed.
	if fileSize > int64(maxMultipartPutObjectSize) {
		return 0, ErrEntityTooLarge(fileSize, maxMultipartPutObjectSize, bucketName, objectName)
	}

	// Set contentType based on filepath extension if not given or default
	// value of "binary/octet-stream" if the extension has no associated type.
	if contentType == "" {
		if contentType = mime.TypeByExtension(filepath.Ext(filePath)); contentType == "" {
			contentType = "application/octet-stream"
		}
	}

	// NOTE: Google Cloud Storage multipart Put is not compatible with Amazon S3 APIs.
	// Current implementation will only upload a maximum of 5GiB to Google Cloud Storage servers.
	if isGoogleEndpoint(c.endpointURL) {
		if fileSize > int64(maxSinglePutObjectSize) {
			return 0, ErrorResponse{
				Code:       "NotImplemented",
				Message:    fmt.Sprintf("Invalid Content-Length %d for file uploads to Google Cloud Storage.", fileSize),
				Key:        objectName,
				BucketName: bucketName,
			}
		}
		// Do not compute MD5 for Google Cloud Storage. Uploads up to 5GiB in size.
		return c.putObjectNoChecksum(bucketName, objectName, fileReader, fileSize, contentType, nil)
	}

	// NOTE: S3 doesn't allow anonymous multipart requests.
	if isAmazonEndpoint(c.endpointURL) && c.anonymous {
		if fileSize > int64(maxSinglePutObjectSize) {
			return 0, ErrorResponse{
				Code:       "NotImplemented",
				Message:    fmt.Sprintf("For anonymous requests Content-Length cannot be %d.", fileSize),
				Key:        objectName,
				BucketName: bucketName,
			}
		}
		// Do not compute MD5 for anonymous requests to Amazon
		// S3. Uploads up to 5GiB in size.
		return c.putObjectNoChecksum(bucketName, objectName, fileReader, fileSize, contentType, nil)
	}

	// Small object upload is initiated for uploads for input data size smaller than 5MiB.
	if fileSize < minPartSize && fileSize >= 0 {
		return c.putObjectSingle(bucketName, objectName, fileReader, fileSize, contentType, nil)
	}
	// Upload all large objects as multipart.
	n, err = c.putObjectMultipartFromFile(bucketName, objectName, fileReader, fileSize, contentType, nil)
	if err != nil {
		errResp := ToErrorResponse(err)
		// Verify if multipart functionality is not available, if not
		// fall back to single PutObject operation.
		if errResp.Code == "NotImplemented" {
			// If size of file is greater than '5GiB' fail.
			if fileSize > maxSinglePutObjectSize {
				return 0, ErrEntityTooLarge(fileSize, maxSinglePutObjectSize, bucketName, objectName)
			}
			// Fall back to uploading as single PutObject operation.
			return c.putObjectSingle(bucketName, objectName, fileReader, fileSize, contentType, nil)
		}
		return n, err
	}
	return n, nil
}

// putObjectMultipartFromFile - Creates object from contents of *os.File
//
// NOTE: This function is meant to be used for readers with local
// file as in *os.File. This function resumes by skipping all the
// necessary parts which were already uploaded by verifying them
// against MD5SUM of each individual parts. This function also
// effectively utilizes file system capabilities of reading from
// specific sections and not having to create temporary files.
func (c Client) putObjectMultipartFromFile(bucketName, objectName string, fileReader io.ReaderAt, fileSize int64, contentType string, progress io.Reader) (int64, error) {
	// Input validation.
	if err := isValidBucketName(bucketName); err != nil {
		return 0, err
	}
	if err := isValidObjectName(objectName); err != nil {
		return 0, err
	}

	// Get upload id for an object, initiates a new multipart request
	// if it cannot find any previously partially uploaded object.
	uploadID, isNew, err := c.getUploadID(bucketName, objectName, contentType)
	if err != nil {
		return 0, err
	}

	// Total data read and written to server. should be equal to 'size' at the end of the call.
	var totalUploadedSize int64

	// Complete multipart upload.
	var complMultipartUpload completeMultipartUpload

	// A map of all uploaded parts.
	var partsInfo = make(map[int]objectPart)

	// If this session is a continuation of a previous session fetch all
	// previously uploaded parts info.
	if !isNew {
		// Fetch previously upload parts and maximum part size.
		partsInfo, err = c.listObjectParts(bucketName, objectName, uploadID)
		if err != nil {
			return 0, err
		}
	}

	// Calculate the optimal parts info for a given size.
	totalPartsCount, partSize, lastPartSize, err := optimalPartInfo(fileSize)
	if err != nil {
		return 0, err
	}

	// Create a channel to communicate a part was uploaded.
	// Buffer this to 10000, the maximum number of parts allowed by S3.
	uploadedPartsCh := make(chan uploadedPartRes, maxPartsCount)

	// Create a channel to communicate which part to upload.
	// Buffer this to 10000, the maximum number of parts allowed by S3.
	uploadPartsCh := make(chan int, maxPartsCount)

	// Just for readability.
	lastPartNumber := totalPartsCount

	// Use three 'workers' to upload parts in parallel.
	for w := 1; w <= 3; w++ {
		go func() {
			// Deal with each part as it comes through the channel.
			for partNumber := range uploadPartsCh {
				// Get a section reader on a particular offset.
				sectionReader := io.NewSectionReader(fileReader, totalUploadedSize, partSize)

				// Add hash algorithms that need to be calculated by computeHash()
				// In case of a non-v4 signature or https connection, sha256 is not needed.
				hashAlgos := make(map[string]hash.Hash)
				hashSums := make(map[string][]byte)
				hashAlgos["md5"] = md5.New()
				if c.signature.isV4() && !c.secure {
					hashAlgos["sha256"] = sha256.New()
				}

				// Create the part to be uploaded.
				verifyObjPart := objectPart{
					ETag:       hex.EncodeToString(hashSums["md5"]),
					PartNumber: partNumber,
					Size:       partSize,
				}
				// If this is the last part do not give it the full part size.
				if partNumber == lastPartNumber {
					verifyObjPart.Size = lastPartSize
				}

				var prtSize int64
				prtSize, err = computeHash(hashAlgos, hashSums, sectionReader)
				if err != nil {
					uploadedPartsCh <- uploadedPartRes{
						error: err,
					}
					// Exit the goroutine.
					return
				}

				// Verify if part should be uploaded.
				if shouldUploadPart(verifyObjPart, partsInfo) {
					// Proceed to upload the part.
					var objPart objectPart
					objPart, err = c.uploadPart(bucketName, objectName, uploadID, sectionReader, partNumber, hashSums["md5"], hashSums["sha256"], prtSize)
					if err != nil {
						uploadedPartsCh <- uploadedPartRes{
							error: err,
						}
						// Exit the goroutine.
						return
					}
					// Save successfully uploaded part metadata.
					partsInfo[partNumber] = objPart
				}
				// Return through the channel the part size.
				uploadedPartsCh <- uploadedPartRes{verifyObjPart, nil}
			}
		}()
	}

	// Send each part through the partUploadCh to be uploaded.
	for p := 1; p <= totalPartsCount; p++ {
		uploadPartsCh <- p
	}
	// Close here closes the worker go-routines.
	close(uploadPartsCh)

	// Gather the responses as they occur and update any progress bar.
	complMultipartUpload.Parts = make([]completePart, totalPartsCount)
	for u := 1; u <= totalPartsCount; u++ {
		uploadRes := <-uploadedPartsCh
		if uploadRes.error != nil {
			return totalUploadedSize, uploadRes.error
		}
		// Update the total uploaded size.
		totalUploadedSize += uploadRes.Size
		// Update the progress bar if there is one.
		if progress != nil {
			if _, err = io.CopyN(ioutil.Discard, progress, uploadRes.Size); err != nil {
				return totalUploadedSize, err
			}
		}
		part, ok := partsInfo[uploadRes.PartNumber]
		if !ok {
			return totalUploadedSize, ErrInvalidArgument(fmt.Sprintf("Missing part number %d", uploadRes.PartNumber))
		}
		// Store the parts to be completed in order.
		complMultipartUpload.Parts[uploadRes.PartNumber-1] = completePart{
			ETag:       part.ETag,
			PartNumber: part.PartNumber,
		}
	}

	// Verify if we uploaded all data.
	if totalUploadedSize != fileSize {
		return totalUploadedSize, ErrUnexpectedEOF(totalUploadedSize, fileSize, bucketName, objectName)
	}

	// Sort all completed parts.
	sort.Sort(completedParts(complMultipartUpload.Parts))
	_, err = c.completeMultipartUpload(bucketName, objectName, uploadID, complMultipartUpload)
	if err != nil {
		return totalUploadedSize, err
	}

	// Return final size.
	return totalUploadedSize, nil
}
