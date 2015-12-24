/*
 * Minio Go Library for Amazon S3 Compatible Cloud Storage (C) 2015 Minio, Inc.
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
	"os"
	"sort"
)

// getUploadID if already present for object name or initiate a request to fetch a new upload id.
func (c Client) getUploadID(bucketName, objectName, contentType string) (string, error) {
	// Input validation.
	if err := isValidBucketName(bucketName); err != nil {
		return "", err
	}
	if err := isValidObjectName(objectName); err != nil {
		return "", err
	}

	// Set content Type to default if empty string.
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	// Find upload id for previous upload for an object.
	uploadID, err := c.findUploadID(bucketName, objectName)
	if err != nil {
		return "", err
	}
	if uploadID == "" {
		// Initiate multipart upload for an object.
		initMultipartUploadResult, err := c.initiateMultipartUpload(bucketName, objectName, contentType)
		if err != nil {
			return "", err
		}
		// Save the new upload id.
		uploadID = initMultipartUploadResult.UploadID
	}
	return uploadID, nil
}

// getMultipartStat gather next part number, total uploaded size and list of parts.
// TODO: reduce number of results.
func (c Client) getMultipartStat(bucketName, objectName, uploadID string) (int, int64, completeMultipartUpload, error) {
	// Input validation.
	if err := isValidBucketName(bucketName); err != nil {
		return 0, 0, completeMultipartUpload{}, err
	}
	if err := isValidObjectName(objectName); err != nil {
		return 0, 0, completeMultipartUpload{}, err
	}
	if uploadID == "" {
		return 0, 0, completeMultipartUpload{}, ErrInvalidArgument("Upload id cannot be empty.")
	}

	// total data read and written to server. should be equal to 'size' at the end of the call.
	var totalUploadedSize int64

	// Starting part number. Always part '1'.
	partNumber := 1
	completeMultipartUpload := completeMultipartUpload{}

	// Done channel is used to communicate with the go routine inside listObjectParts.
	// It is necessary to close dangling routines inside once we break out of the loop.
	doneCh := make(chan struct{})
	// Close listObjectParts channel by communicating that we are done.
	defer close(doneCh)

	for objPart := range c.listObjectParts(bucketName, objectName, uploadID, doneCh) {
		if objPart.Err != nil {
			return 0, 0, completeMultipartUpload, objPart.Err
		}
		// Verify if there is a hole i.e one of the parts is missing
		// Break and start uploading from this part.
		if partNumber != objPart.PartNumber {
			break
		}
		var completedPart completePart
		completedPart.PartNumber = objPart.PartNumber
		completedPart.ETag = objPart.ETag
		completeMultipartUpload.Parts = append(completeMultipartUpload.Parts, completedPart)
		// Save total uploaded size which will be incremented later.
		totalUploadedSize += objPart.Size
		// Increment additively to verify holes in next iteration.
		partNumber++
	}

	// Return.
	return partNumber, totalUploadedSize, completeMultipartUpload, nil
}

// FPutObject - put object a file.
func (c Client) FPutObject(bucketName, objectName, filePath, contentType string) (int64, error) {
	// Input validation.
	if err := isValidBucketName(bucketName); err != nil {
		return 0, err
	}
	if err := isValidObjectName(objectName); err != nil {
		return 0, err
	}

	// Open the referenced file.
	fileData, err := os.Open(filePath)
	// If any error fail quickly here.
	if err != nil {
		return 0, err
	}

	// Save the file stat.
	fileStat, err := fileData.Stat()
	if err != nil {
		return 0, err
	}

	// Save the file size.
	fileSize := fileStat.Size()
	var enableSha256Sum bool
	if !c.signature.isV2() {
		enableSha256Sum = true
	}

	// getUploadID for an object, initiates a new request if necessary.
	uploadID, err := c.getUploadID(bucketName, objectName, contentType)
	if err != nil {
		return 0, err
	}

	// gather next part number to be uploaded, total uploaded size and list of all parts uploaded.
	partNumber, totalUploadedSize, completeMultipartUpload, err := c.getMultipartStat(bucketName, objectName, uploadID)
	if err != nil {
		return 0, err
	}

	// Calculate the optimal part size for a given file size.
	partSize := calculatePartSize(fileSize)

	// Seek to the total uploaded size obtained after listing all the parts.
	if totalUploadedSize > 0 {
		if _, err := fileData.Seek(totalUploadedSize, 0); err != nil {
			return 0, err
		}
	}

	// Make done channel.
	doneCh := make(chan struct{})
	// Close done channel to indicate closure to all go-routines.
	defer close(doneCh)

	// Loop through all sections of the file.
	for part := range sectionManager(fileData, fileSize, partSize, enableSha256Sum, doneCh) {
		// On any error return.
		if part.Err != nil {
			return 0, part.Err
		}

		// Part number to be uploaded.
		part.Number = partNumber

		// execute upload part.
		complPart, err := c.uploadPart(bucketName, objectName, uploadID, part)
		if err != nil {
			part.ReadCloser.Close()
			return totalUploadedSize, err
		}

		// Save successfully uploaded size.
		totalUploadedSize += part.Size

		// Save successfully uploaded part metadatc.
		completeMultipartUpload.Parts = append(completeMultipartUpload.Parts, complPart)

		// Increment to next part number.
		partNumber++
	}

	// if totalUploadedSize is different than the file 'size'. Do not complete the request throw an error.
	if totalUploadedSize != fileSize {
		return totalUploadedSize, ErrUnexpectedEOF(totalUploadedSize, fileSize, bucketName, objectName)
	}

	// Sort all completed parts.
	sort.Sort(completedParts(completeMultipartUpload.Parts))
	_, err = c.completeMultipartUpload(bucketName, objectName, uploadID, completeMultipartUpload)
	if err != nil {
		return totalUploadedSize, err
	}

	// Return final size.
	return totalUploadedSize, nil
}