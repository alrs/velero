/*
Copyright 2018 the Heptio Ark contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/heptio/ark/pkg/apis/ark/v1"
	pkgbackup "github.com/heptio/ark/pkg/backup"
	"github.com/heptio/ark/pkg/generated/clientset/versioned/fake"
	informers "github.com/heptio/ark/pkg/generated/informers/externalversions"
	"github.com/heptio/ark/pkg/util/kube"
	arktest "github.com/heptio/ark/pkg/util/test"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	core "k8s.io/client-go/testing"
)

func TestBackupDeletionControllerControllerHasUpdateFunc(t *testing.T) {
	req := pkgbackup.NewDeleteBackupRequest("foo", "uid")
	req.Namespace = "heptio-ark"
	expected := kube.NamespaceAndName(req)

	client := fake.NewSimpleClientset(req)

	fakeWatch := watch.NewFake()
	defer fakeWatch.Stop()
	client.PrependWatchReactor("deletebackuprequests", core.DefaultWatchReactor(fakeWatch, nil))

	sharedInformers := informers.NewSharedInformerFactory(client, 0)

	controller := NewBackupDeletionController(
		arktest.NewLogger(),
		sharedInformers.Ark().V1().DeleteBackupRequests(),
		client.ArkV1(), // deleteBackupRequestClient
		client.ArkV1(), // backupClient
		nil,            // snapshotService
		nil,            // backupService
		"bucket",
		sharedInformers.Ark().V1().Restores(),
		client.ArkV1(), // restoreClient
	).(*backupDeletionController)

	// disable resync handler since we don't want to test it here
	controller.resyncFunc = nil

	keys := make(chan string)

	controller.syncHandler = func(key string) error {
		keys <- key
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	go sharedInformers.Start(ctx.Done())
	go controller.Run(ctx, 1)

	// wait for the AddFunc
	select {
	case <-ctx.Done():
		t.Fatal("test timed out waiting for AddFunc")
	case key := <-keys:
		assert.Equal(t, expected, key)
	}

	req.Status.Phase = v1.DeleteBackupRequestPhaseProcessed
	fakeWatch.Add(req)

	// wait for the UpdateFunc
	select {
	case <-ctx.Done():
		t.Fatal("test timed out waiting for UpdateFunc")
	case key := <-keys:
		assert.Equal(t, expected, key)
	}
}

func TestBackupDeletionControllerProcessQueueItem(t *testing.T) {
	client := fake.NewSimpleClientset()
	sharedInformers := informers.NewSharedInformerFactory(client, 0)

	controller := NewBackupDeletionController(
		arktest.NewLogger(),
		sharedInformers.Ark().V1().DeleteBackupRequests(),
		client.ArkV1(), // deleteBackupRequestClient
		client.ArkV1(), // backupClient
		nil,            // snapshotService
		nil,            // backupService
		"bucket",
		sharedInformers.Ark().V1().Restores(),
		client.ArkV1(), // restoreClient
	).(*backupDeletionController)

	// Error splitting key
	err := controller.processQueueItem("foo/bar/baz")
	assert.Error(t, err)

	// Can't find DeleteBackupRequest
	err = controller.processQueueItem("foo/bar")
	assert.NoError(t, err)

	// Already processed
	req := pkgbackup.NewDeleteBackupRequest("foo", "uid")
	req.Namespace = "foo"
	req.Name = "foo-abcde"
	req.Status.Phase = v1.DeleteBackupRequestPhaseProcessed

	err = controller.processQueueItem("foo/bar")
	assert.NoError(t, err)

	// Invoke processRequestFunc
	for _, phase := range []v1.DeleteBackupRequestPhase{"", v1.DeleteBackupRequestPhaseNew, v1.DeleteBackupRequestPhaseInProgress} {
		t.Run(fmt.Sprintf("phase=%s", phase), func(t *testing.T) {
			req.Status.Phase = phase
			sharedInformers.Ark().V1().DeleteBackupRequests().Informer().GetStore().Add(req)

			var errorToReturn error
			var actual *v1.DeleteBackupRequest
			var called bool
			controller.processRequestFunc = func(r *v1.DeleteBackupRequest) error {
				called = true
				actual = r
				return errorToReturn
			}

			// No error
			err = controller.processQueueItem("foo/foo-abcde")
			require.True(t, called, "processRequestFunc wasn't called")
			assert.Equal(t, err, errorToReturn)
			assert.Equal(t, req, actual)

			// Error
			errorToReturn = errors.New("bar")
			err = controller.processQueueItem("foo/foo-abcde")
			require.True(t, called, "processRequestFunc wasn't called")
			assert.Equal(t, err, errorToReturn)
		})
	}
}

type backupDeletionControllerTestData struct {
	client          *fake.Clientset
	sharedInformers informers.SharedInformerFactory
	backupService   *arktest.BackupService
	snapshotService *arktest.FakeSnapshotService
	controller      *backupDeletionController
	req             *v1.DeleteBackupRequest
}

func setupBackupDeletionControllerTest(objects ...runtime.Object) *backupDeletionControllerTestData {
	client := fake.NewSimpleClientset(objects...)
	sharedInformers := informers.NewSharedInformerFactory(client, 0)
	backupService := &arktest.BackupService{}
	snapshotService := &arktest.FakeSnapshotService{SnapshotsTaken: sets.NewString()}
	req := pkgbackup.NewDeleteBackupRequest("foo", "uid")

	data := &backupDeletionControllerTestData{
		client:          client,
		sharedInformers: sharedInformers,
		backupService:   backupService,
		snapshotService: snapshotService,
		controller: NewBackupDeletionController(
			arktest.NewLogger(),
			sharedInformers.Ark().V1().DeleteBackupRequests(),
			client.ArkV1(), // deleteBackupRequestClient
			client.ArkV1(), // backupClient
			snapshotService,
			backupService,
			"bucket",
			sharedInformers.Ark().V1().Restores(),
			client.ArkV1(), // restoreClient
		).(*backupDeletionController),

		req: req,
	}
	req.Namespace = "heptio-ark"
	req.Name = "foo-abcde"

	return data
}

func TestBackupDeletionControllerProcessRequest(t *testing.T) {
	t.Run("patching to InProgress fails", func(t *testing.T) {
		td := setupBackupDeletionControllerTest()
		defer td.backupService.AssertExpectations(t)

		td.client.PrependReactor("patch", "deletebackuprequests", func(action core.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New("bad")
		})

		err := td.controller.processRequest(td.req)
		assert.EqualError(t, err, "error patching DeleteBackupRequest: bad")
	})

	t.Run("unable to find backup", func(t *testing.T) {
		td := setupBackupDeletionControllerTest()
		defer td.backupService.AssertExpectations(t)

		td.client.PrependReactor("get", "backups", func(action core.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(v1.SchemeGroupVersion.WithResource("backups").GroupResource(), "foo")
		})

		td.client.PrependReactor("patch", "deletebackuprequests", func(action core.Action) (bool, runtime.Object, error) {
			return true, td.req, nil
		})

		err := td.controller.processRequest(td.req)
		require.NoError(t, err)

		expectedActions := []core.Action{
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				[]byte(`{"status":{"phase":"InProgress"}}`),
			),
			core.NewGetAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				[]byte(`{"status":{"errors":["backup not found"],"phase":"Processed"}}`),
			),
		}

		assert.Equal(t, expectedActions, td.client.Actions())
	})

	t.Run("no snapshot service, backup has snapshots", func(t *testing.T) {
		td := setupBackupDeletionControllerTest()
		td.controller.snapshotService = nil
		defer td.backupService.AssertExpectations(t)

		td.client.PrependReactor("get", "backups", func(action core.Action) (bool, runtime.Object, error) {
			backup := arktest.NewTestBackup().WithName("backup-1").WithSnapshot("pv-1", "snap-1").Backup
			return true, backup, nil
		})

		td.client.PrependReactor("patch", "deletebackuprequests", func(action core.Action) (bool, runtime.Object, error) {
			return true, td.req, nil
		})

		err := td.controller.processRequest(td.req)
		require.NoError(t, err)

		expectedActions := []core.Action{
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				[]byte(`{"status":{"phase":"InProgress"}}`),
			),
			core.NewGetAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				[]byte(`{"status":{"errors":["unable to delete backup because it includes PV snapshots and Ark is not configured with a PersistentVolumeProvider"],"phase":"Processed"}}`),
			),
		}

		assert.Equal(t, expectedActions, td.client.Actions())
	})

	t.Run("full delete, no errors", func(t *testing.T) {
		backup := arktest.NewTestBackup().WithName("foo").WithSnapshot("pv-1", "snap-1").Backup
		backup.UID = "uid"

		restore1 := arktest.NewTestRestore("heptio-ark", "restore-1", v1.RestorePhaseCompleted).WithBackup("foo").Restore
		restore2 := arktest.NewTestRestore("heptio-ark", "restore-2", v1.RestorePhaseCompleted).WithBackup("foo").Restore
		restore3 := arktest.NewTestRestore("heptio-ark", "restore-3", v1.RestorePhaseCompleted).WithBackup("some-other-backup").Restore

		td := setupBackupDeletionControllerTest(backup, restore1, restore2, restore3)

		td.sharedInformers.Ark().V1().Restores().Informer().GetStore().Add(restore1)
		td.sharedInformers.Ark().V1().Restores().Informer().GetStore().Add(restore2)
		td.sharedInformers.Ark().V1().Restores().Informer().GetStore().Add(restore3)

		defer td.backupService.AssertExpectations(t)

		td.client.PrependReactor("get", "backups", func(action core.Action) (bool, runtime.Object, error) {
			return true, backup, nil
		})
		td.snapshotService.SnapshotsTaken.Insert("snap-1")

		td.client.PrependReactor("patch", "deletebackuprequests", func(action core.Action) (bool, runtime.Object, error) {
			return true, td.req, nil
		})

		td.client.PrependReactor("patch", "backups", func(action core.Action) (bool, runtime.Object, error) {
			return true, backup, nil
		})

		td.backupService.On("DeleteBackupDir", td.controller.bucket, td.req.Spec.BackupName).Return(nil)

		err := td.controller.processRequest(td.req)
		require.NoError(t, err)

		expectedActions := []core.Action{
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				[]byte(`{"status":{"phase":"InProgress"}}`),
			),
			core.NewGetAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
				[]byte(`{"status":{"phase":"Deleting"}}`),
			),
			core.NewDeleteAction(
				v1.SchemeGroupVersion.WithResource("restores"),
				td.req.Namespace,
				"restore-1",
			),
			core.NewDeleteAction(
				v1.SchemeGroupVersion.WithResource("restores"),
				td.req.Namespace,
				"restore-2",
			),
			core.NewDeleteAction(
				v1.SchemeGroupVersion.WithResource("backups"),
				td.req.Namespace,
				td.req.Spec.BackupName,
			),
			core.NewPatchAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				td.req.Name,
				[]byte(`{"status":{"phase":"Processed"}}`),
			),
			core.NewDeleteCollectionAction(
				v1.SchemeGroupVersion.WithResource("deletebackuprequests"),
				td.req.Namespace,
				pkgbackup.NewDeleteBackupRequestListOptions(td.req.Spec.BackupName, "uid"),
			),
		}

		assert.Len(t, td.client.Actions(), len(expectedActions))
		for _, e := range expectedActions {
			found := false
			for _, a := range td.client.Actions() {
				if reflect.DeepEqual(e, a) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("missing expected action %#v", e)
			}
		}

		// Make sure snapshot was deleted
		assert.Equal(t, 0, td.snapshotService.SnapshotsTaken.Len())
	})
}

func TestBackupDeletionControllerDeleteExpiredRequests(t *testing.T) {
	now := time.Date(2018, 4, 4, 12, 0, 0, 0, time.UTC)
	unexpired1 := time.Date(2018, 4, 4, 11, 0, 0, 0, time.UTC)
	unexpired2 := time.Date(2018, 4, 3, 12, 0, 1, 0, time.UTC)
	expired1 := time.Date(2018, 4, 3, 12, 0, 0, 0, time.UTC)
	expired2 := time.Date(2018, 4, 3, 2, 0, 0, 0, time.UTC)

	tests := []struct {
		name              string
		requests          []*v1.DeleteBackupRequest
		expectedDeletions []string
	}{
		{
			name: "no requests",
		},
		{
			name: "older than max age, phase = '', don't delete",
			requests: []*v1.DeleteBackupRequest{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "name",
						CreationTimestamp: metav1.Time{Time: expired1},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: "",
					},
				},
			},
		},
		{
			name: "older than max age, phase = New, don't delete",
			requests: []*v1.DeleteBackupRequest{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "name",
						CreationTimestamp: metav1.Time{Time: expired1},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: v1.DeleteBackupRequestPhaseNew,
					},
				},
			},
		},
		{
			name: "older than max age, phase = InProcess, don't delete",
			requests: []*v1.DeleteBackupRequest{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "name",
						CreationTimestamp: metav1.Time{Time: expired1},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: v1.DeleteBackupRequestPhaseInProgress,
					},
				},
			},
		},
		{
			name: "some expired, some not",
			requests: []*v1.DeleteBackupRequest{
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "unexpired-1",
						CreationTimestamp: metav1.Time{Time: unexpired1},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: v1.DeleteBackupRequestPhaseProcessed,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "expired-1",
						CreationTimestamp: metav1.Time{Time: expired1},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: v1.DeleteBackupRequestPhaseProcessed,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "unexpired-2",
						CreationTimestamp: metav1.Time{Time: unexpired2},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: v1.DeleteBackupRequestPhaseProcessed,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Namespace:         "ns",
						Name:              "expired-2",
						CreationTimestamp: metav1.Time{Time: expired2},
					},
					Status: v1.DeleteBackupRequestStatus{
						Phase: v1.DeleteBackupRequestPhaseProcessed,
					},
				},
			},
			expectedDeletions: []string{"expired-1", "expired-2"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			sharedInformers := informers.NewSharedInformerFactory(client, 0)

			controller := NewBackupDeletionController(
				arktest.NewLogger(),
				sharedInformers.Ark().V1().DeleteBackupRequests(),
				client.ArkV1(), // deleteBackupRequestClient
				client.ArkV1(), // backupClient
				nil,            // snapshotService
				nil,            // backupService
				"bucket",
				sharedInformers.Ark().V1().Restores(),
				client.ArkV1(), // restoreClient
			).(*backupDeletionController)

			fakeClock := &clock.FakeClock{}
			fakeClock.SetTime(now)
			controller.clock = fakeClock

			for i := range test.requests {
				sharedInformers.Ark().V1().DeleteBackupRequests().Informer().GetStore().Add(test.requests[i])
			}

			controller.deleteExpiredRequests()

			expectedActions := []core.Action{}
			for _, name := range test.expectedDeletions {
				expectedActions = append(expectedActions, core.NewDeleteAction(v1.SchemeGroupVersion.WithResource("deletebackuprequests"), "ns", name))
			}

			assert.Equal(t, expectedActions, client.Actions())
		})

	}
}
