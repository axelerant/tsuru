// Copyright 2015 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package docker

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/fsouza/go-dockerclient"
	"github.com/fsouza/go-dockerclient/testing"
	"github.com/tsuru/config"
	"github.com/tsuru/docker-cluster/cluster"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/log"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/provision/docker/container"
	"gopkg.in/check.v1"
	"gopkg.in/mgo.v2/bson"
)

func checkContainerInContainerSlices(c container.Container, cList []container.Container) error {
	for _, cont := range cList {
		if cont.ID == c.ID {
			return nil
		}
	}
	return errors.New("container is not in list")
}

func (s *S) TestSchedulerSchedule(c *check.C) {
	a1 := app.App{Name: "impius", Teams: []string{"tsuruteam", "nodockerforme"}, Pool: "pool1"}
	a2 := app.App{Name: "mirror", Teams: []string{"tsuruteam"}, Pool: "pool1"}
	a3 := app.App{Name: "dedication", Teams: []string{"nodockerforme"}, Pool: "pool1"}
	cont1 := container.Container{ID: "1", Name: "impius1", AppName: a1.Name}
	cont2 := container.Container{ID: "2", Name: "mirror1", AppName: a2.Name}
	cont3 := container.Container{ID: "3", Name: "dedication1", AppName: a3.Name}
	err := s.storage.Apps().Insert(a1, a2, a3)
	c.Assert(err, check.IsNil)
	defer s.storage.Apps().RemoveAll(bson.M{"name": bson.M{"$in": []string{a1.Name, a2.Name, a3.Name}}})
	p := provision.Pool{Name: "pool1", Teams: []string{
		"tsuruteam",
		"nodockerforme",
	}}
	o := provision.AddPoolOptions{Name: p.Name}
	err = provision.AddPool(o)
	c.Assert(err, check.IsNil)
	err = provision.AddTeamsToPool(p.Name, p.Teams)
	defer provision.RemovePool(p.Name)
	contColl := s.p.Collection()
	defer contColl.Close()
	err = contColl.Insert(
		cont1, cont2, cont3,
	)
	c.Assert(err, check.IsNil)
	defer contColl.RemoveAll(bson.M{"name": bson.M{"$in": []string{cont1.Name, cont2.Name, cont3.Name}}})
	scheduler := segregatedScheduler{provisioner: s.p}
	clusterInstance, err := cluster.New(&scheduler, &cluster.MapStorage{})
	s.p.cluster = clusterInstance
	c.Assert(err, check.IsNil)
	server1, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer server1.Stop()
	server2, err := testing.NewServer("localhost:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer server2.Stop()
	err = clusterInstance.Register(cluster.Node{
		Address:  server1.URL(),
		Metadata: map[string]string{"pool": "pool1"},
	})
	c.Assert(err, check.IsNil)
	localURL := strings.Replace(server2.URL(), "127.0.0.1", "localhost", -1)
	err = clusterInstance.Register(cluster.Node{
		Address:  localURL,
		Metadata: map[string]string{"pool": "pool1"},
	})
	c.Assert(err, check.IsNil)
	opts := docker.CreateContainerOptions{Name: cont1.Name}
	node, err := scheduler.Schedule(clusterInstance, opts, []string{a1.Name, "web"})
	c.Assert(err, check.IsNil)
	c.Check(node.Address, check.Equals, server1.URL())
	opts = docker.CreateContainerOptions{Name: cont2.Name}
	node, err = scheduler.Schedule(clusterInstance, opts, []string{a2.Name, "web"})
	c.Assert(err, check.IsNil)
	c.Check(node.Address, check.Equals, localURL)
}

func (s *S) TestSchedulerScheduleByTeamOwner(c *check.C) {
	a1 := app.App{Name: "impius", Teams: []string{}, TeamOwner: "tsuruteam"}
	cont1 := container.Container{ID: "1", Name: "impius1", AppName: a1.Name}
	err := s.storage.Apps().Insert(a1)
	c.Assert(err, check.IsNil)
	defer s.storage.Apps().RemoveAll(bson.M{"name": a1.Name})
	p := provision.Pool{Name: "pool1", Teams: []string{"tsuruteam"}}
	o := provision.AddPoolOptions{Name: p.Name}
	err = provision.AddPool(o)
	c.Assert(err, check.IsNil)
	defer provision.RemovePool(p.Name)
	err = provision.AddTeamsToPool(p.Name, p.Teams)
	c.Assert(err, check.IsNil)
	contColl := s.p.Collection()
	defer contColl.Close()
	err = contColl.Insert(cont1)
	c.Assert(err, check.IsNil)
	defer contColl.RemoveAll(bson.M{"name": cont1.Name})
	scheduler := segregatedScheduler{provisioner: s.p}
	clusterInstance, err := cluster.New(&scheduler, &cluster.MapStorage{})
	s.p.cluster = clusterInstance
	c.Assert(err, check.IsNil)
	err = clusterInstance.Register(cluster.Node{
		Address:  s.server.URL(),
		Metadata: map[string]string{"pool": "pool1"},
	})
	c.Assert(err, check.IsNil)
	opts := docker.CreateContainerOptions{Name: cont1.Name}
	node, err := scheduler.Schedule(clusterInstance, opts, []string{a1.Name, "web"})
	c.Assert(err, check.IsNil)
	c.Check(node.Address, check.Equals, s.server.URL())
}

func (s *S) TestSchedulerScheduleByTeams(c *check.C) {
	a1 := app.App{Name: "impius", Teams: []string{"tsuruteam", "nopool"}}
	cont1 := container.Container{ID: "1", Name: "impius1", AppName: a1.Name}
	err := s.storage.Apps().Insert(a1)
	c.Assert(err, check.IsNil)
	defer s.storage.Apps().RemoveAll(bson.M{"name": a1.Name})
	p := provision.Pool{Name: "pool1", Teams: []string{"tsuruteam"}}
	o := provision.AddPoolOptions{Name: p.Name}
	err = provision.AddPool(o)
	c.Assert(err, check.IsNil)
	defer provision.RemovePool(p.Name)
	err = provision.AddTeamsToPool(p.Name, p.Teams)
	c.Assert(err, check.IsNil)
	contColl := s.p.Collection()
	defer contColl.Close()
	err = contColl.Insert(cont1)
	c.Assert(err, check.IsNil)
	defer contColl.RemoveAll(bson.M{"name": cont1.Name})
	scheduler := segregatedScheduler{provisioner: s.p}
	clusterInstance, err := cluster.New(&scheduler, &cluster.MapStorage{})
	s.p.cluster = clusterInstance
	c.Assert(err, check.IsNil)
	err = clusterInstance.Register(cluster.Node{
		Address:  s.server.URL(),
		Metadata: map[string]string{"pool": "pool1"},
	})
	c.Assert(err, check.IsNil)
	opts := docker.CreateContainerOptions{Name: cont1.Name}
	node, err := scheduler.Schedule(clusterInstance, opts, []string{a1.Name, "web"})
	c.Assert(err, check.IsNil)
	c.Check(node.Address, check.Equals, s.server.URL())
}

func (s *S) TestSchedulerScheduleNoName(c *check.C) {
	a1 := app.App{Name: "impius", Teams: []string{"tsuruteam", "nodockerforme"}, Pool: "pool1"}
	a2 := app.App{Name: "mirror", Teams: []string{"tsuruteam"}, Pool: "pool1"}
	a3 := app.App{Name: "dedication", Teams: []string{"nodockerforme"}, Pool: "pool1"}
	cont1 := container.Container{ID: "1", Name: "impius1", AppName: a1.Name}
	cont2 := container.Container{ID: "2", Name: "mirror1", AppName: a2.Name}
	cont3 := container.Container{ID: "3", Name: "dedication1", AppName: a3.Name}
	err := s.storage.Apps().Insert(a1, a2, a3)
	c.Assert(err, check.IsNil)
	defer s.storage.Apps().RemoveAll(bson.M{"name": bson.M{"$in": []string{a1.Name, a2.Name, a3.Name}}})
	p := provision.Pool{Name: "pool1", Teams: []string{
		"tsuruteam",
		"nodockerforme",
	}}
	o := provision.AddPoolOptions{Name: p.Name}
	err = provision.AddPool(o)
	c.Assert(err, check.IsNil)
	err = provision.AddTeamsToPool(p.Name, p.Teams)
	defer provision.RemovePool(p.Name)
	contColl := s.p.Collection()
	defer contColl.Close()
	err = contColl.Insert(
		cont1, cont2, cont3,
	)
	c.Assert(err, check.IsNil)
	defer contColl.RemoveAll(bson.M{"name": bson.M{"$in": []string{cont1.Name, cont2.Name, cont3.Name}}})
	scheduler := segregatedScheduler{provisioner: s.p}
	clusterInstance, err := cluster.New(&scheduler, &cluster.MapStorage{})
	s.p.cluster = clusterInstance
	c.Assert(err, check.IsNil)
	server1, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer server1.Stop()
	server2, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer server2.Stop()
	localURL := strings.Replace(server2.URL(), "127.0.0.1", "localhost", -1)
	err = clusterInstance.Register(cluster.Node{
		Address:  server1.URL(),
		Metadata: map[string]string{"pool": "pool1"},
	})
	c.Assert(err, check.IsNil)
	err = clusterInstance.Register(cluster.Node{
		Address:  localURL,
		Metadata: map[string]string{"pool": "pool1"},
	})
	c.Assert(err, check.IsNil)
	opts := docker.CreateContainerOptions{}
	node, err := scheduler.Schedule(clusterInstance, opts, []string{a1.Name, "web"})
	c.Assert(err, check.IsNil)
	c.Check(node.Address, check.Equals, server1.URL())
	container, err := s.p.GetContainer(cont1.ID)
	c.Assert(err, check.IsNil)
	c.Assert(container.HostAddr, check.Equals, "")
}

func (s *S) TestSchedulerScheduleDefaultPool(c *check.C) {
	a1 := app.App{Name: "impius", Teams: []string{"tsuruteam", "nodockerforme"}}
	cont1 := container.Container{ID: "1", Name: "impius1", AppName: a1.Name}
	err := s.storage.Apps().Insert(a1)
	c.Assert(err, check.IsNil)
	defer s.storage.Apps().RemoveAll(bson.M{"name": a1.Name})
	contColl := s.p.Collection()
	defer contColl.Close()
	err = contColl.Insert(cont1)
	c.Assert(err, check.IsNil)
	defer contColl.RemoveAll(bson.M{"name": cont1.Name})
	scheduler := segregatedScheduler{provisioner: s.p}
	clusterInstance, err := cluster.New(&scheduler, &cluster.MapStorage{})
	s.p.cluster = clusterInstance
	c.Assert(err, check.IsNil)
	err = clusterInstance.Register(cluster.Node{
		Address:  s.server.URL(),
		Metadata: map[string]string{"pool": "test-default"},
	})
	c.Assert(err, check.IsNil)
	opts := docker.CreateContainerOptions{Name: cont1.Name}
	node, err := scheduler.Schedule(clusterInstance, opts, []string{a1.Name, "web"})
	c.Assert(err, check.IsNil)
	c.Check(node.Address, check.Equals, s.server.URL())
}

func (s *S) TestSchedulerNoDefaultPool(c *check.C) {
	provision.RemovePool("test-default")
	a := app.App{Name: "bill", Teams: []string{"jean"}}
	err := s.storage.Apps().Insert(a)
	c.Assert(err, check.IsNil)
	defer s.storage.Apps().Remove(bson.M{"name": a.Name})
	cont1 := container.Container{ID: "1", Name: "bill", AppName: a.Name, ProcessName: "web"}
	contColl := s.p.Collection()
	defer contColl.Close()
	err = contColl.Insert(cont1)
	c.Assert(err, check.IsNil)
	defer contColl.Remove(bson.M{"name": cont1.Name})
	scheduler := segregatedScheduler{provisioner: s.p}
	clusterInstance, err := cluster.New(&scheduler, &cluster.MapStorage{})
	c.Assert(err, check.IsNil)
	opts := docker.CreateContainerOptions{Name: cont1.Name}
	schedOpts := []string{a.Name, "web"}
	node, err := scheduler.Schedule(clusterInstance, opts, schedOpts)
	c.Assert(node.Address, check.Equals, "")
	c.Assert(err, check.NotNil)
	c.Assert(err, check.Equals, errNoDefaultPool)
}

func (s *S) TestSchedulerNoNodesNoPool(c *check.C) {
	provision.RemovePool("test-default")
	app := app.App{Name: "bill", Teams: []string{"jean"}}
	err := s.storage.Apps().Insert(app)
	c.Assert(err, check.IsNil)
	defer s.storage.Apps().Remove(bson.M{"name": app.Name})
	scheduler := segregatedScheduler{provisioner: s.p}
	clusterInstance, err := cluster.New(&scheduler, &cluster.MapStorage{})
	c.Assert(err, check.IsNil)
	opts := docker.CreateContainerOptions{}
	schedOpts := []string{app.Name, "web"}
	node, err := scheduler.Schedule(clusterInstance, opts, schedOpts)
	c.Assert(node.Address, check.Equals, "")
	c.Assert(err, check.NotNil)
	c.Assert(err, check.Equals, errNoDefaultPool)
}

func (s *S) TestSchedulerNoNodesWithDefaultPool(c *check.C) {
	provision.RemovePool("test-default")
	app := app.App{Name: "bill", Teams: []string{"jean"}}
	err := s.storage.Apps().Insert(app)
	c.Assert(err, check.IsNil)
	defer s.storage.Apps().Remove(bson.M{"name": app.Name})
	scheduler := segregatedScheduler{provisioner: s.p}
	clusterInstance, err := cluster.New(&scheduler, &cluster.MapStorage{})
	c.Assert(err, check.IsNil)
	o := provision.AddPoolOptions{Name: "mypool"}
	err = provision.AddPool(o)
	c.Assert(err, check.IsNil)
	o = provision.AddPoolOptions{Name: "mypool2"}
	err = provision.AddPool(o)
	c.Assert(err, check.IsNil)
	defer provision.RemovePool("mypool")
	defer provision.RemovePool("mypool2")
	provision.AddTeamsToPool("mypool", []string{"jean"})
	provision.AddTeamsToPool("mypool2", []string{"jean"})
	opts := docker.CreateContainerOptions{}
	schedOpts := []string{app.Name, "web"}
	node, err := scheduler.Schedule(clusterInstance, opts, schedOpts)
	c.Assert(node.Address, check.Equals, "")
	c.Assert(err, check.NotNil)
	c.Assert(err.Error(), check.Matches, "No nodes found with one of the following metadata: pool=mypool, pool=mypool2")
}

func (s *S) TestSchedulerScheduleWithMemoryAwareness(c *check.C) {
	logBuf := bytes.NewBuffer(nil)
	log.SetLogger(log.NewWriterLogger(logBuf, false))
	defer log.SetLogger(nil)
	app1 := app.App{Name: "skyrim", Plan: app.Plan{Memory: 60000}, Pool: "mypool"}
	err := s.storage.Apps().Insert(app1)
	c.Assert(err, check.IsNil)
	defer s.storage.Apps().Remove(bson.M{"name": app1.Name})
	app2 := app.App{Name: "oblivion", Plan: app.Plan{Memory: 20000}, Pool: "mypool"}
	err = s.storage.Apps().Insert(app2)
	c.Assert(err, check.IsNil)
	defer s.storage.Apps().Remove(bson.M{"name": app2.Name})
	segSched := segregatedScheduler{
		maxMemoryRatio:      0.8,
		TotalMemoryMetadata: "totalMemory",
		provisioner:         s.p,
	}
	o := provision.AddPoolOptions{Name: "mypool"}
	err = provision.AddPool(o)
	c.Assert(err, check.IsNil)
	defer provision.RemovePool("mypool")
	server1, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer server1.Stop()
	server2, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer server2.Stop()
	localURL := strings.Replace(server2.URL(), "127.0.0.1", "localhost", -1)
	clusterInstance, err := cluster.New(&segSched, &cluster.MapStorage{},
		cluster.Node{Address: server1.URL(), Metadata: map[string]string{
			"totalMemory": "100000",
			"pool":        "mypool",
		}},
		cluster.Node{Address: localURL, Metadata: map[string]string{
			"totalMemory": "100000",
			"pool":        "mypool",
		}},
	)
	c.Assert(err, check.Equals, nil)
	s.p.cluster = clusterInstance
	cont1 := container.Container{ID: "pre1", Name: "existingUnit1", AppName: "skyrim", HostAddr: "127.0.0.1"}
	contColl := s.p.Collection()
	defer contColl.Close()
	defer contColl.RemoveAll(bson.M{"appname": "skyrim"})
	defer contColl.RemoveAll(bson.M{"appname": "oblivion"})
	err = contColl.Insert(cont1)
	c.Assert(err, check.Equals, nil)
	for i := 0; i < 5; i++ {
		cont := container.Container{ID: string(i), Name: fmt.Sprintf("unit%d", i), AppName: "oblivion"}
		err := contColl.Insert(cont)
		c.Assert(err, check.IsNil)
		opts := docker.CreateContainerOptions{
			Name: cont.Name,
		}
		node, err := segSched.Schedule(clusterInstance, opts, []string{cont.AppName, "web"})
		c.Assert(err, check.IsNil)
		c.Assert(node, check.NotNil)
	}
	n, err := contColl.Find(bson.M{"hostaddr": "127.0.0.1"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 2)
	n, err = contColl.Find(bson.M{"hostaddr": "localhost"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 4)
	n, err = contColl.Find(bson.M{"hostaddr": "127.0.0.1", "appname": "oblivion"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 1)
	n, err = contColl.Find(bson.M{"hostaddr": "localhost", "appname": "oblivion"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 4)
	cont := container.Container{ID: "post-error", Name: "post-error-1", AppName: "oblivion"}
	err = contColl.Insert(cont)
	c.Assert(err, check.IsNil)
	opts := docker.CreateContainerOptions{
		Name: cont.Name,
	}
	node, err := segSched.Schedule(clusterInstance, opts, []string{cont.AppName, "web"})
	c.Assert(err, check.ErrorMatches, `.*no nodes found with enough memory for container of "oblivion": 0.0191MB.*`)
	c.Assert(node, check.DeepEquals, cluster.Node{})
}

func (s *S) TestSchedulerScheduleWithMemoryAwarenessWithAutoScale(c *check.C) {
	config.Set("docker:auto-scale:enabled", true)
	defer config.Unset("docker:auto-scale:enabled")
	logBuf := bytes.NewBuffer(nil)
	log.SetLogger(log.NewWriterLogger(logBuf, false))
	defer log.SetLogger(nil)
	app1 := app.App{Name: "skyrim", Plan: app.Plan{Memory: 60000}, Pool: "mypool"}
	err := s.storage.Apps().Insert(app1)
	c.Assert(err, check.IsNil)
	defer s.storage.Apps().Remove(bson.M{"name": app1.Name})
	app2 := app.App{Name: "oblivion", Plan: app.Plan{Memory: 20000}, Pool: "mypool"}
	err = s.storage.Apps().Insert(app2)
	c.Assert(err, check.IsNil)
	defer s.storage.Apps().Remove(bson.M{"name": app2.Name})
	segSched := segregatedScheduler{
		maxMemoryRatio:      0.8,
		TotalMemoryMetadata: "totalMemory",
		provisioner:         s.p,
	}
	o := provision.AddPoolOptions{Name: "mypool"}
	err = provision.AddPool(o)
	c.Assert(err, check.IsNil)
	defer provision.RemovePool("mypool")
	server1, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer server1.Stop()
	server2, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer server2.Stop()
	localURL := strings.Replace(server2.URL(), "127.0.0.1", "localhost", -1)
	clusterInstance, err := cluster.New(&segSched, &cluster.MapStorage{},
		cluster.Node{Address: server1.URL(), Metadata: map[string]string{
			"totalMemory": "100000",
			"pool":        "mypool",
		}},
		cluster.Node{Address: localURL, Metadata: map[string]string{
			"totalMemory": "100000",
			"pool":        "mypool",
		}},
	)
	c.Assert(err, check.Equals, nil)
	s.p.cluster = clusterInstance
	cont1 := container.Container{ID: "pre1", Name: "existingUnit1", AppName: "skyrim", HostAddr: "127.0.0.1"}
	contColl := s.p.Collection()
	defer contColl.Close()
	defer contColl.RemoveAll(bson.M{"appname": "skyrim"})
	defer contColl.RemoveAll(bson.M{"appname": "oblivion"})
	err = contColl.Insert(cont1)
	c.Assert(err, check.Equals, nil)
	for i := 0; i < 5; i++ {
		cont := container.Container{ID: string(i), Name: fmt.Sprintf("unit%d", i), AppName: "oblivion"}
		err := contColl.Insert(cont)
		c.Assert(err, check.IsNil)
		opts := docker.CreateContainerOptions{
			Name: cont.Name,
		}
		node, err := segSched.Schedule(clusterInstance, opts, []string{cont.AppName, "web"})
		c.Assert(err, check.IsNil)
		c.Assert(node, check.NotNil)
	}
	n, err := contColl.Find(bson.M{"hostaddr": "127.0.0.1"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 2)
	n, err = contColl.Find(bson.M{"hostaddr": "localhost"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 4)
	n, err = contColl.Find(bson.M{"hostaddr": "127.0.0.1", "appname": "oblivion"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 1)
	n, err = contColl.Find(bson.M{"hostaddr": "localhost", "appname": "oblivion"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 4)
	cont := container.Container{ID: "post-error", Name: "post-error-1", AppName: "oblivion"}
	err = contColl.Insert(cont)
	c.Assert(err, check.IsNil)
	opts := docker.CreateContainerOptions{
		Name: cont.Name,
	}
	node, err := segSched.Schedule(clusterInstance, opts, []string{cont.AppName, "web"})
	c.Assert(err, check.IsNil)
	c.Assert(node, check.NotNil)
	c.Assert(logBuf.String(), check.Matches, `(?s).*WARNING: no nodes found with enough memory for container of "oblivion": 0.0191MB.*`)
}

func (s *S) TestChooseNodeDistributesNodesEqually(c *check.C) {
	nodes := []cluster.Node{
		{Address: "http://server1:1234"},
		{Address: "http://server2:1234"},
		{Address: "http://server3:1234"},
		{Address: "http://server4:1234"},
	}
	contColl := s.p.Collection()
	defer contColl.Close()
	defer contColl.RemoveAll(bson.M{"appname": "coolapp9"})
	cont1 := container.Container{ID: "pre1", Name: "existingUnit1", AppName: "coolapp9", HostAddr: "server1"}
	err := contColl.Insert(cont1)
	c.Assert(err, check.Equals, nil)
	cont2 := container.Container{ID: "pre2", Name: "existingUnit2", AppName: "coolapp9", HostAddr: "server2"}
	err = contColl.Insert(cont2)
	c.Assert(err, check.Equals, nil)
	numberOfUnits := 38
	unitsPerNode := (numberOfUnits + 2) / 4
	wg := sync.WaitGroup{}
	wg.Add(numberOfUnits)
	sched := segregatedScheduler{provisioner: s.p}
	for i := 0; i < numberOfUnits; i++ {
		go func(i int) {
			defer wg.Done()
			cont := container.Container{ID: string(i), Name: fmt.Sprintf("unit%d", i), AppName: "coolapp9"}
			err := contColl.Insert(cont)
			c.Assert(err, check.IsNil)
			node, err := sched.chooseNode(nodes, cont.Name, "coolapp9", "web")
			c.Assert(err, check.IsNil)
			c.Assert(node, check.NotNil)
		}(i)
	}
	wg.Wait()
	n, err := contColl.Find(bson.M{"hostaddr": "server1"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, unitsPerNode)
	n, err = contColl.Find(bson.M{"hostaddr": "server2"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, unitsPerNode)
	n, err = contColl.Find(bson.M{"hostaddr": "server3"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, unitsPerNode)
	n, err = contColl.Find(bson.M{"hostaddr": "server4"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, unitsPerNode)
}

func (s *S) TestChooseNodeDistributesNodesEquallyDifferentApps(c *check.C) {
	nodes := []cluster.Node{
		{Address: "http://server1:1234"},
		{Address: "http://server2:1234"},
	}
	contColl := s.p.Collection()
	defer contColl.Close()
	defer contColl.RemoveAll(bson.M{"appname": "skyrim"})
	defer contColl.RemoveAll(bson.M{"appname": "oblivion"})
	cont1 := container.Container{ID: "pre1", Name: "existingUnit1", AppName: "skyrim", HostAddr: "server1", ProcessName: "web"}
	err := contColl.Insert(cont1)
	c.Assert(err, check.IsNil)
	cont2 := container.Container{ID: "pre2", Name: "existingUnit2", AppName: "skyrim", HostAddr: "server1", ProcessName: "web"}
	err = contColl.Insert(cont2)
	c.Assert(err, check.IsNil)
	cont3 := container.Container{ID: "pre3", Name: "existingUnit3", AppName: "skyrim", HostAddr: "server1", ProcessName: "web"}
	err = contColl.Insert(cont3)
	c.Assert(err, check.IsNil)
	numberOfUnits := 2
	wg := sync.WaitGroup{}
	wg.Add(numberOfUnits)
	sched := segregatedScheduler{provisioner: s.p}
	for i := 0; i < numberOfUnits; i++ {
		go func(i int) {
			defer wg.Done()
			cont := container.Container{ID: string(i), Name: fmt.Sprintf("unit%d", i), AppName: "oblivion", ProcessName: "web"}
			err := contColl.Insert(cont)
			c.Assert(err, check.IsNil)
			node, err := sched.chooseNode(nodes, cont.Name, "oblivion", "web")
			c.Assert(err, check.IsNil)
			c.Assert(node, check.NotNil)
		}(i)
	}
	wg.Wait()
	n, err := contColl.Find(bson.M{"hostaddr": "server1"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 4)
	n, err = contColl.Find(bson.M{"hostaddr": "server2"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 1)
	n, err = contColl.Find(bson.M{"hostaddr": "server1", "appname": "oblivion"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 1)
	n, err = contColl.Find(bson.M{"hostaddr": "server2", "appname": "oblivion"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 1)
}

func (s *S) TestChooseNodeDistributesNodesEquallyDifferentProcesses(c *check.C) {
	nodes := []cluster.Node{
		{Address: "http://server1:1234"},
		{Address: "http://server2:1234"},
	}
	contColl := s.p.Collection()
	defer contColl.Close()
	defer contColl.RemoveAll(bson.M{"appname": "skyrim"})
	cont1 := container.Container{ID: "pre1", Name: "existingUnit1", AppName: "skyrim", HostAddr: "server1", ProcessName: "web"}
	err := contColl.Insert(cont1)
	c.Assert(err, check.Equals, nil)
	cont2 := container.Container{ID: "pre2", Name: "existingUnit2", AppName: "skyrim", HostAddr: "server1", ProcessName: "web"}
	err = contColl.Insert(cont2)
	c.Assert(err, check.Equals, nil)
	numberOfUnits := 2
	wg := sync.WaitGroup{}
	wg.Add(numberOfUnits)
	sched := segregatedScheduler{provisioner: s.p}
	for i := 0; i < numberOfUnits; i++ {
		go func(i int) {
			defer wg.Done()
			cont := container.Container{ID: string(i), Name: fmt.Sprintf("unit%d", i), AppName: "skyrim", ProcessName: "worker"}
			err := contColl.Insert(cont)
			c.Assert(err, check.IsNil)
			node, err := sched.chooseNode(nodes, cont.Name, "skyrim", "worker")
			c.Assert(err, check.IsNil)
			c.Assert(node, check.NotNil)
		}(i)
	}
	wg.Wait()
	n, err := contColl.Find(bson.M{"hostaddr": "server1"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 3)
	n, err = contColl.Find(bson.M{"hostaddr": "server2"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 1)
	n, err = contColl.Find(bson.M{"hostaddr": "server1", "processname": "worker"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 1)
	n, err = contColl.Find(bson.M{"hostaddr": "server2", "processname": "worker"}).Count()
	c.Assert(err, check.Equals, nil)
	c.Check(n, check.Equals, 1)
}

func (s *S) TestChooseContainerToBeRemoved(c *check.C) {
	nodes := []cluster.Node{
		{Address: "http://server1:1234"},
		{Address: "http://server2:1234"},
	}
	contColl := s.p.Collection()
	defer contColl.Close()
	defer contColl.RemoveAll(bson.M{"appname": "coolapp9"})
	cont1 := container.Container{
		ID:          "pre1",
		Name:        "existingUnit1",
		AppName:     "coolapp9",
		HostAddr:    "server1",
		ProcessName: "web",
	}
	err := contColl.Insert(cont1)
	c.Assert(err, check.Equals, nil)
	cont2 := container.Container{
		ID:          "pre2",
		Name:        "existingUnit2",
		AppName:     "coolapp9",
		HostAddr:    "server2",
		ProcessName: "web",
	}
	err = contColl.Insert(cont2)
	c.Assert(err, check.Equals, nil)
	cont3 := container.Container{
		ID:          "pre3",
		Name:        "existingUnit1",
		AppName:     "coolapp9",
		HostAddr:    "server1",
		ProcessName: "web",
	}
	err = contColl.Insert(cont3)
	c.Assert(err, check.Equals, nil)
	scheduler := segregatedScheduler{provisioner: s.p}
	containerID, err := scheduler.chooseContainerFromMaxContainersCountInNode(nodes, "coolapp9", "web")
	c.Assert(err, check.IsNil)
	c.Assert(containerID, check.Equals, "pre1")
}

func (s *S) TestAggregateContainersByHostAppProcess(c *check.C) {
	contColl := s.p.Collection()
	defer contColl.Close()
	cont := container.Container{ID: "pre1", AppName: "app1", HostAddr: "server1", ProcessName: "web"}
	err := contColl.Insert(cont)
	c.Assert(err, check.IsNil)
	cont = container.Container{ID: "pre2", AppName: "app1", HostAddr: "server1", ProcessName: ""}
	err = contColl.Insert(cont)
	c.Assert(err, check.IsNil)
	cont = container.Container{ID: "pre3", AppName: "app2", HostAddr: "server1", ProcessName: ""}
	err = contColl.Insert(cont)
	c.Assert(err, check.IsNil)
	cont = container.Container{ID: "pre4", AppName: "app1", HostAddr: "server2", ProcessName: ""}
	err = contColl.Insert(cont)
	c.Assert(err, check.IsNil)
	err = contColl.Insert(map[string]string{"id": "pre5", "appname": "app1", "hostaddr": "server2"})
	c.Assert(err, check.IsNil)
	scheduler := segregatedScheduler{provisioner: s.p}
	result, err := scheduler.aggregateContainersByHostAppProcess([]string{"server1", "server2"}, "app1", "")
	c.Assert(err, check.IsNil)
	c.Assert(result, check.DeepEquals, map[string]int{"server1": 1, "server2": 2})
}

func (s *S) TestChooseContainerToBeRemovedMultipleProcesses(c *check.C) {
	nodes := []cluster.Node{
		{Address: "http://server1:1234"},
		{Address: "http://server2:1234"},
	}
	contColl := s.p.Collection()
	defer contColl.Close()
	cont1 := container.Container{ID: "pre1", AppName: "coolapp9", HostAddr: "server1", ProcessName: "web"}
	err := contColl.Insert(cont1)
	c.Assert(err, check.IsNil)
	cont2 := container.Container{ID: "pre2", AppName: "coolapp9", HostAddr: "server1", ProcessName: "web"}
	err = contColl.Insert(cont2)
	c.Assert(err, check.IsNil)
	cont3 := container.Container{ID: "pre3", AppName: "coolapp9", HostAddr: "server1", ProcessName: "web"}
	err = contColl.Insert(cont3)
	c.Assert(err, check.IsNil)
	cont4 := container.Container{ID: "pre4", AppName: "coolapp9", HostAddr: "server1", ProcessName: ""}
	err = contColl.Insert(cont4)
	c.Assert(err, check.IsNil)
	cont5 := container.Container{ID: "pre5", AppName: "coolapp9", HostAddr: "server2", ProcessName: ""}
	err = contColl.Insert(cont5)
	c.Assert(err, check.IsNil)
	err = contColl.Insert(map[string]string{"id": "pre6", "appname": "coolapp9", "hostaddr": "server2"})
	c.Assert(err, check.IsNil)
	scheduler := segregatedScheduler{provisioner: s.p}
	containerID, err := scheduler.chooseContainerFromMaxContainersCountInNode(nodes, "coolapp9", "")
	c.Assert(err, check.IsNil)
	c.Assert(containerID == "pre5" || containerID == "pre6", check.Equals, true)
}

func (s *S) TestGetContainerFromHost(c *check.C) {
	contColl := s.p.Collection()
	defer contColl.Close()
	defer contColl.RemoveAll(bson.M{"appname": "coolapp9"})
	cont1 := container.Container{
		ID:          "pre1",
		Name:        "existingUnit1",
		AppName:     "coolapp9",
		HostAddr:    "server1",
		ProcessName: "some",
	}
	err := contColl.Insert(cont1)
	c.Assert(err, check.Equals, nil)
	scheduler := segregatedScheduler{provisioner: s.p}
	id, err := scheduler.getContainerFromHost("server1", "coolapp9", "some")
	c.Assert(err, check.IsNil)
	c.Assert(id, check.Equals, "pre1")
	_, err = scheduler.getContainerFromHost("server2", "coolapp9", "some")
	c.Assert(err, check.NotNil)
	_, err = scheduler.getContainerFromHost("server1", "coolapp9", "other")
	c.Assert(err, check.NotNil)
	_, err = scheduler.getContainerFromHost("server1", "coolapp8", "some")
	c.Assert(err, check.NotNil)
}

func (s *S) TestGetContainerFromHostEmptyProcess(c *check.C) {
	contColl := s.p.Collection()
	defer contColl.Close()
	err := contColl.Insert(map[string]string{"id": "pre1", "name": "unit1", "appname": "coolappX", "hostaddr": "server1"})
	c.Assert(err, check.Equals, nil)
	err = contColl.Insert(map[string]string{"id": "pre2", "name": "unit1", "appname": "coolappX", "hostaddr": "server2", "processname": ""})
	c.Assert(err, check.Equals, nil)
	scheduler := segregatedScheduler{provisioner: s.p}
	id, err := scheduler.getContainerFromHost("server1", "coolappX", "")
	c.Assert(err, check.IsNil)
	c.Assert(id, check.Equals, "pre1")
	id, err = scheduler.getContainerFromHost("server2", "coolappX", "")
	c.Assert(err, check.IsNil)
	c.Assert(id, check.Equals, "pre2")
	_, err = scheduler.getContainerFromHost("server1", "coolappX", "other")
	c.Assert(err, check.NotNil)
}

func (s *S) TestGetRemovableContainer(c *check.C) {
	a1 := app.App{Name: "impius", Teams: []string{"tsuruteam", "nodockerforme"}, Pool: "pool1"}
	cont1 := container.Container{ID: "1", Name: "impius1", AppName: a1.Name, ProcessName: "web"}
	cont2 := container.Container{ID: "2", Name: "mirror1", AppName: a1.Name, ProcessName: "worker"}
	a2 := app.App{Name: "notimpius", Teams: []string{"tsuruteam", "nodockerforme"}, Pool: "pool1"}
	cont3 := container.Container{ID: "3", Name: "dedication1", AppName: a2.Name, ProcessName: "web"}
	cont4 := container.Container{ID: "4", Name: "dedication2", AppName: a2.Name, ProcessName: "worker"}
	err := s.storage.Apps().Insert(a1)
	c.Assert(err, check.IsNil)
	err = s.storage.Apps().Insert(a2)
	c.Assert(err, check.IsNil)
	defer s.storage.Apps().RemoveAll(bson.M{"name": a1.Name})
	defer s.storage.Apps().RemoveAll(bson.M{"name": a2.Name})
	p := provision.Pool{Name: "pool1", Teams: []string{
		"tsuruteam",
		"nodockerforme",
	}}
	o := provision.AddPoolOptions{Name: p.Name}
	err = provision.AddPool(o)
	c.Assert(err, check.IsNil)
	err = provision.AddTeamsToPool(p.Name, p.Teams)
	defer provision.RemovePool(p.Name)
	contColl := s.p.Collection()
	defer contColl.Close()
	err = contColl.Insert(
		cont1, cont2, cont3, cont4,
	)
	c.Assert(err, check.IsNil)
	defer contColl.RemoveAll(bson.M{"name": bson.M{"$in": []string{cont1.Name, cont2.Name, cont3.Name, cont4.Name}}})
	scheduler := segregatedScheduler{provisioner: s.p}
	clusterInstance, err := cluster.New(&scheduler, &cluster.MapStorage{})
	s.p.cluster = clusterInstance
	c.Assert(err, check.IsNil)
	server1, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer server1.Stop()
	server2, err := testing.NewServer("127.0.0.1:0", nil, nil)
	c.Assert(err, check.IsNil)
	defer server2.Stop()
	localURL := strings.Replace(server2.URL(), "127.0.0.1", "localhost", -1)
	err = clusterInstance.Register(cluster.Node{
		Address:  server1.URL(),
		Metadata: map[string]string{"pool": "pool1"},
	})
	c.Assert(err, check.IsNil)
	err = clusterInstance.Register(cluster.Node{
		Address:  localURL,
		Metadata: map[string]string{"pool": "pool1"},
	})
	c.Assert(err, check.IsNil)
	opts := docker.CreateContainerOptions{Name: cont1.Name}
	_, err = scheduler.Schedule(clusterInstance, opts, []string{a1.Name, cont1.ProcessName})
	c.Assert(err, check.IsNil)
	opts = docker.CreateContainerOptions{Name: cont2.Name}
	_, err = scheduler.Schedule(clusterInstance, opts, []string{a1.Name, cont2.ProcessName})
	c.Assert(err, check.IsNil)
	opts = docker.CreateContainerOptions{Name: cont3.Name}
	_, err = scheduler.Schedule(clusterInstance, opts, []string{a2.Name, cont3.ProcessName})
	c.Assert(err, check.IsNil)
	opts = docker.CreateContainerOptions{Name: cont4.Name}
	_, err = scheduler.Schedule(clusterInstance, opts, []string{a2.Name, cont4.ProcessName})
	c.Assert(err, check.IsNil)
	cont, err := scheduler.GetRemovableContainer(a1.Name, "web")
	c.Assert(err, check.IsNil)
	c.Assert(cont, check.Equals, cont1.ID)
	err = cont1.Remove(s.p)
	c.Assert(err, check.IsNil)
	_, err = scheduler.GetRemovableContainer(a1.Name, "web")
	c.Assert(err, check.NotNil)
}

func (s *S) TestNodesToHosts(c *check.C) {
	nodes := []cluster.Node{
		{Address: "http://server1:1234"},
		{Address: "http://server2:1234"},
	}
	scheduler := segregatedScheduler{provisioner: s.p}
	hosts, hostsMap := scheduler.nodesToHosts(nodes)
	c.Assert(hosts, check.NotNil)
	c.Assert(hostsMap, check.NotNil)
	c.Assert(len(hosts), check.Equals, 2)
	c.Assert(hostsMap[hosts[0]], check.Equals, nodes[0].Address)
}

func (s *S) TestChooseContainerToBeRemovedMultipleApps(c *check.C) {
	nodes := []cluster.Node{
		{Address: "http://server1:1234"},
		{Address: "http://server2:1234"},
	}
	contColl := s.p.Collection()
	defer contColl.Close()
	cont1 := container.Container{ID: "pre1", AppName: "coolapp1", HostAddr: "server1"}
	err := contColl.Insert(cont1)
	c.Assert(err, check.IsNil)
	cont2 := container.Container{ID: "pre2", AppName: "coolapp1", HostAddr: "server1"}
	err = contColl.Insert(cont2)
	c.Assert(err, check.IsNil)
	cont3 := container.Container{ID: "pre3", AppName: "coolapp1", HostAddr: "server1"}
	err = contColl.Insert(cont3)
	c.Assert(err, check.IsNil)
	cont4 := container.Container{ID: "pre4", AppName: "coolapp2", HostAddr: "server1"}
	err = contColl.Insert(cont4)
	c.Assert(err, check.IsNil)
	cont5 := container.Container{ID: "pre5", AppName: "coolapp2", HostAddr: "server2"}
	err = contColl.Insert(cont5)
	c.Assert(err, check.IsNil)
	cont6 := container.Container{ID: "pre6", AppName: "coolapp2", HostAddr: "server2"}
	err = contColl.Insert(cont6)
	c.Assert(err, check.IsNil)
	scheduler := segregatedScheduler{provisioner: s.p}
	containerID, err := scheduler.chooseContainerFromMaxContainersCountInNode(nodes, "coolapp2", "")
	c.Assert(err, check.IsNil)
	c.Assert(containerID == "pre5" || containerID == "pre6", check.Equals, true)
}
