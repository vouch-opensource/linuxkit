package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/s3"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

const timeoutVar = "LINUXKIT_UPLOAD_TIMEOUT"

const minSizeForMultipartUpload int64 = 4 * 1024 * 1024 * 1024 // 4Gib

const multipartUploadPartSize int64 = 1024 * 1024 * 1024 // 1Gib

func pushAWSCmd() *cobra.Command {
	var (
		timeoutFlag int
		bucketFlag  string
		nameFlag    string
		ena         bool
		sriovNet    string
		uefi        bool
		tpm         bool
	)
	cmd := &cobra.Command{
		Use:   "aws",
		Short: "push image to AWS",
		Long: `Push image to AWS.
		Single argument specifies the full path of an AWS image. It will be uploaded to S3 and an AMI will be created from it.
		`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := args[0]

			timeout := getIntValue(timeoutVar, timeoutFlag, 600)
			bucket := getStringValue(bucketVar, bucketFlag, "")
			name := getStringValue(nameVar, nameFlag, "")

			var sriovNetFlag *string
			if sriovNet != "" {
				*sriovNetFlag = sriovNet
			}

			if !uefi && tpm {
				return fmt.Errorf("Cannot use tpm without uefi mode")
			}

			sess := session.Must(session.NewSession())
			storage := s3.New(sess)

			ctx, cancelFn := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
			defer cancelFn()

			if bucket == "" {
				return fmt.Errorf("Please provide the bucket to use")
			}

			f, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("Error opening file: %v", err)
			}
			defer f.Close()

			if name == "" {
				name = strings.TrimSuffix(path, filepath.Ext(path))
				name = filepath.Base(name)
			}

			fi, err := f.Stat()
			if err != nil {
				return fmt.Errorf("Error reading file information: %v", err)
			}

			dst := name + filepath.Ext(path)

			// check if the file size is less than the minimum part size
			// if it is, then just do a regular upload
			fileSize := fi.Size()

			if *aws.Int64(fileSize) <= minSizeForMultipartUpload {
				log.Debugf("Using regular upload for file with size %d", fileSize)

				putParams := &s3.PutObjectInput{
					Bucket:        aws.String(bucket),
					Key:           aws.String(dst),
					Body:          f,
					ContentLength: aws.Int64(fileSize),
					ContentType:   aws.String("application/octet-stream"),
				}

				log.Debugf("PutObject:\n%v", putParams)

				_, err = storage.PutObjectWithContext(ctx, putParams)
				if err != nil {
					return fmt.Errorf("Error uploading to S3: %v", err)
				}
			} else {
				log.Debugf("Using multipart upload for file with size %d", fileSize)

				//struct for starting a multipart upload
				startInput := s3.CreateMultipartUploadInput{
					Bucket: aws.String(bucket),
					Key:    aws.String(dst),
				}

				var uploadId string
				createOutput, err := storage.CreateMultipartUploadWithContext(ctx, &startInput)
				if err != nil {
					return err
				}
				if createOutput != nil {
					if createOutput.UploadId != nil {
						uploadId = *createOutput.UploadId
					}
				}
				if uploadId == "" {
					return fmt.Errorf("No upload id found in start upload request: %v", err)
				}

				// var partNumber int64
				var numUploads int = int(math.Ceil(float64(*aws.Int64(fileSize)) / float64(multipartUploadPartSize)))
				parts := make([]*s3.CompletedPart, numUploads)

				log.Infof("Will attempt upload in %d number of parts to %s", numUploads, *aws.String(dst))

				for partNumber := 0; partNumber < numUploads; partNumber++ {
					// Calculate the byte range for this part
					start := int64(partNumber) * multipartUploadPartSize
					end := int64(math.Min(float64(start+multipartUploadPartSize), float64(fileSize)))
					length := end - start
					rangeStr := fmt.Sprintf("bytes %d-%d/%d", start, end-1, fileSize)

					log.Debugf("Attempting to upload part %d with range %s", partNumber, rangeStr)

					// Read the part data
					partData := make([]byte, length)
					_, err := f.ReadAt(partData, start)
					if err != nil {
						return err
					}

					partInput := &s3.UploadPartInput{
						Bucket:     aws.String(bucket),
						Key:        aws.String(dst),
						Body:       bytes.NewReader(partData),
						PartNumber: aws.Int64(int64(partNumber) + 1),
						UploadId:   aws.String(uploadId),
					}
					log.Debugf("Attempting to upload part %d", partNumber)
					partResp, err := storage.UploadPart(partInput)

					if err != nil {
						log.Error("Attempting to abort upload")
						abortIn := s3.AbortMultipartUploadInput{
							UploadId: aws.String(uploadId),
						}
						//ignoring any errors with aborting the copy
						storage.AbortMultipartUploadRequest(&abortIn)
						return fmt.Errorf("Error uploading part %d : %w", partNumber, err)
					}

					// Save the completed part
					parts[partNumber] = &s3.CompletedPart{
						ETag:       partResp.ETag,
						PartNumber: aws.Int64(int64(partNumber) + 1),
					}
				}

				//create struct for completing the upload
				mpu := s3.CompletedMultipartUpload{
					Parts: parts,
				}

				//complete actual upload
				//does not actually copy if the complete command is not received
				complete := s3.CompleteMultipartUploadInput{
					Bucket:          aws.String(bucket),
					Key:             aws.String(dst),
					UploadId:        aws.String(uploadId),
					MultipartUpload: &mpu,
				}
				compOutput, err := storage.CompleteMultipartUpload(&complete)
				if err != nil {
					return fmt.Errorf("Error completing upload: %w", err)
				}
				if compOutput != nil {
					log.Infof("Successfully uploaded Key: %s to Bucket: %s", *aws.String(dst), *aws.String(dst))
				}
			}

			compute := ec2.New(sess)

			importParams := &ec2.ImportSnapshotInput{
				Description: aws.String(fmt.Sprintf("LinuxKit: %s", name)),
				DiskContainer: &ec2.SnapshotDiskContainer{
					Description: aws.String(fmt.Sprintf("LinuxKit: %s disk", name)),
					Format:      aws.String("raw"),
					UserBucket: &ec2.UserBucket{
						S3Bucket: aws.String(bucket),
						S3Key:    aws.String(dst),
					},
				},
			}
			log.Debugf("ImportSnapshot:\n%v", importParams)

			resp, err := compute.ImportSnapshot(importParams)
			if err != nil {
				return fmt.Errorf("Error importing snapshot: %v", err)
			}

			var snapshotID *string
			for {
				describeParams := &ec2.DescribeImportSnapshotTasksInput{
					ImportTaskIds: []*string{
						resp.ImportTaskId,
					},
				}
				log.Debugf("DescribeImportSnapshotTask:\n%v", describeParams)
				status, err := compute.DescribeImportSnapshotTasks(describeParams)
				if err != nil {
					return fmt.Errorf("Error getting import snapshot status: %v", err)
				}
				if len(status.ImportSnapshotTasks) == 0 {
					return fmt.Errorf("Unable to get import snapshot task status")
				}
				if *status.ImportSnapshotTasks[0].SnapshotTaskDetail.Status != "completed" {
					progress := "0"
					if status.ImportSnapshotTasks[0].SnapshotTaskDetail.Progress != nil {
						progress = *status.ImportSnapshotTasks[0].SnapshotTaskDetail.Progress
					}
					log.Debugf("Task %s is %s%% complete. Waiting 60 seconds...\n", *resp.ImportTaskId, progress)
					time.Sleep(60 * time.Second)
					continue
				}
				snapshotID = status.ImportSnapshotTasks[0].SnapshotTaskDetail.SnapshotId
				break
			}

			if snapshotID == nil {
				return fmt.Errorf("SnapshotID unavailable after import completed")
			} else {
				log.Debugf("SnapshotID: %s", *snapshotID)
			}

			regParams := &ec2.RegisterImageInput{
				Name:         aws.String(name), // Required
				Architecture: aws.String("x86_64"),
				BlockDeviceMappings: []*ec2.BlockDeviceMapping{
					{
						DeviceName: aws.String("/dev/sda1"),
						Ebs: &ec2.EbsBlockDevice{
							DeleteOnTermination: aws.Bool(true),
							SnapshotId:          snapshotID,
							VolumeType:          aws.String("standard"),
						},
					},
				},
				Description:        aws.String(fmt.Sprintf("LinuxKit: %s image", name)),
				RootDeviceName:     aws.String("/dev/sda1"),
				VirtualizationType: aws.String("hvm"),
				EnaSupport:         &ena,
				SriovNetSupport:    sriovNetFlag,
			}
			if uefi {
				regParams.BootMode = aws.String("uefi")
				if tpm {
					regParams.TpmSupport = aws.String("v2.0")
				}
			}
			log.Debugf("RegisterImage:\n%v", regParams)
			regResp, err := compute.RegisterImage(regParams)
			if err != nil {
				return fmt.Errorf("Error registering the image: %s; %v", name, err)
			}
			log.Infof("Created AMI: %s", *regResp.ImageId)
			return nil
		},
	}

	cmd.Flags().IntVar(&timeoutFlag, "timeout", 0, "Upload timeout in seconds")
	cmd.Flags().StringVar(&bucketFlag, "bucket", "", "S3 Bucket to upload to. *Required*")
	cmd.Flags().StringVar(&nameFlag, "img-name", "", "Overrides the name used to identify the file in Amazon S3 and the VM image. Defaults to the base of 'path' with the file extension removed.")
	cmd.Flags().BoolVar(&ena, "ena", false, "Enable ENA networking")
	cmd.Flags().StringVar(&sriovNet, "sriov", "", "SRIOV network support, set to 'simple' to enable 82599 VF networking")
	cmd.Flags().BoolVar(&uefi, "uefi", false, "Enable uefi boot mode.")
	cmd.Flags().BoolVar(&tpm, "tpm", false, "Enable tpm device.")

	return cmd
}
