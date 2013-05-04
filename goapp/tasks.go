/*
 * Copyright (c) 2013 Matt Jibson <matt.jibson@gmail.com>
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package goapp

import (
	"appengine/datastore"
	"appengine/taskqueue"
	"appengine/urlfetch"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	mpg "github.com/mjibson/MiniProfiler/go/miniprofiler_gae"
	"github.com/mjibson/goon"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

func ImportOpmlTask(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	type outline struct {
		Outline []outline `xml:"outline"`
		Title   string    `xml:"title,attr"`
		XmlUrl  string    `xml:"xmlUrl,attr"`
	}

	type Opml struct {
		XMLName string    `xml:"opml"`
		Outline []outline `xml:"body>outline"`
	}

	gn := goon.FromContext(c)
	userid := r.FormValue("user")
	data := r.FormValue("data")

	var skip int
	if s, err := strconv.Atoi(r.FormValue("skip")); err == nil {
		skip = s
	}
	c.Debugf("reader import for %v, skip %v", userid, skip)

	var ufs []*UserFeed
	remaining := skip

	var proc func(label string, outlines []outline)
	proc = func(label string, outlines []outline) {
		for _, o := range outlines {
			if o.XmlUrl != "" {
				if remaining > 0 {
					remaining--
				} else if len(ufs) < IMPORT_LIMIT {
					ufs = append(ufs, &UserFeed{
						Label: label,
						Url:   o.XmlUrl,
						Title: o.Title,
					})
				}
			}

			if o.Title != "" && len(o.Outline) > 0 {
				proc(o.Title, o.Outline)
			}
		}
	}

	feed := Opml{}
	if err := xml.Unmarshal([]byte(data), &feed); err != nil {
		c.Errorf("opml error: %v", err.Error())
		return
	}
	proc("", feed.Outline)

	// todo: refactor below with similar from ImportReaderTask
	wg := sync.WaitGroup{}
	wg.Add(len(ufs))
	for i := range ufs {
		go func(i int) {
			if err := addFeed(c, userid, ufs[i]); err != nil {
				c.Errorf("opml import error: %v", err.Error())
				// todo: do something here?
			}
			c.Debugf("opml import: %s, %s", ufs[i].Title, ufs[i].Url)
			wg.Done()
		}(i)
	}
	wg.Wait()

	ud := UserData{Id: "data", Parent: gn.Key(&User{Id: userid})}
	if err := gn.RunInTransaction(func(gn *goon.Goon) error {
		gn.Get(&ud)
		addUserFeed(&ud, ufs...)
		return gn.Put(ud)
	}, nil); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		c.Errorf("ude update error: %v", err.Error())
		return
	}

	if len(ufs) == IMPORT_LIMIT {
		task := taskqueue.NewPOSTTask(routeUrl("import-opml-task"), url.Values{
			"data": {data},
			"user": {userid},
			"skip": {strconv.Itoa(skip + IMPORT_LIMIT)},
		})
		taskqueue.Add(c, task, "import-reader")
	}
}

const IMPORT_LIMIT = 20

func ImportReaderTask(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	gn := goon.FromContext(c)
	userid := r.FormValue("user")
	data := r.FormValue("data")

	var skip int
	if s, err := strconv.Atoi(r.FormValue("skip")); err == nil {
		skip = s
	}

	v := struct {
		Subscriptions []struct {
			Id         string `json:"id"`
			Title      string `json:"title"`
			HtmlUrl    string `json:"htmlUrl"`
			Categories []struct {
				Id    string `json:"id"`
				Label string `json:"label"`
			} `json:"categories"`
		} `json:"subscriptions"`
	}{}
	json.Unmarshal([]byte(data), &v)
	c.Debugf("reader import for %v, skip %v, len %v", userid, skip, len(v.Subscriptions))

	end := skip + IMPORT_LIMIT
	if end > len(v.Subscriptions) {
		end = len(v.Subscriptions)
	}

	wg := sync.WaitGroup{}
	wg.Add(end - skip)
	ufs := make([]*UserFeed, end-skip)

	for i := range v.Subscriptions[skip:end] {
		go func(i int) {
			sub := v.Subscriptions[skip+i]
			var label string
			if len(sub.Categories) > 0 {
				label = sub.Categories[0].Label
			}
			uf := &UserFeed{
				Label: label,
				Url:   sub.Id[5:],
				Title: sub.Title,
			}
			ufs[i] = uf
			if err := addFeed(c, userid, uf); err != nil {
				c.Errorf("reader import error: %v", err.Error())
				// todo: do something here?
			}
			c.Debugf("reader import: %s, %s", sub.Title, sub.Id)
			wg.Done()
		}(i)
	}
	wg.Wait()

	ud := UserData{Id: "data", Parent: gn.Key(&User{Id: userid})}
	if err := gn.RunInTransaction(func(gn *goon.Goon) error {
		gn.Get(&ud)
		addUserFeed(&ud, ufs...)
		return gn.Put(&ud)
	}, nil); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		c.Errorf("ude update error: %v", err.Error())
		return
	}

	if end < len(v.Subscriptions) {
		task := taskqueue.NewPOSTTask(routeUrl("import-reader-task"), url.Values{
			"data": {data},
			"user": {userid},
			"skip": {strconv.Itoa(skip + IMPORT_LIMIT)},
		})
		taskqueue.Add(c, task, "import-reader")
	}
}

func UpdateFeeds(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	gn := goon.FromContext(c)
	q := datastore.NewQuery(gn.Key(&Feed{}).Kind()).KeysOnly()
	q = q.Filter("n <=", time.Now())
	keys, _ := gn.GetAll(q, nil)
	for _, k := range keys {
		t := taskqueue.NewPOSTTask(routeUrl("update-feed"), url.Values{
			"feed": {k.StringID()},
		})
		if _, err := taskqueue.Add(c, t, "update-feed"); err != nil {
			c.Errorf("taskqueue error: %v", err.Error())
		}
	}
	c.Infof("updating %d feeds", len(keys))
}

func fetchFeed(c mpg.Context, url string) (*Feed, []*Story) {
	cl := urlfetch.Client(c)
	if resp, err := cl.Get(url); err == nil && resp.StatusCode == http.StatusOK {
		b, _ := ioutil.ReadAll(resp.Body)
		return ParseFeed(c, url, b)
	} else if err != nil {
		c.Errorf("fetch feed error: %s", err.Error())
	} else {
		c.Warningf("fetch feed error: status code: %s", resp.Status)
	}
	return nil, nil
}

func updateFeed(c mpg.Context, url string, feed *Feed, stories []*Story) error {
	gn := goon.FromContext(c)
	f := Feed{Url: url}
	if err := gn.Get(&f); err != nil {
		return errors.New(fmt.Sprintf("feed not found: %s", url))
	}

	// Compare the feed's listed update to the story's update.
	// Note: these may not be accurate, hence, only compare them to each other,
	// since they should have the same relative error.
	storyDate := f.Updated

	hasUpdated := !feed.Updated.IsZero()
	isFeedUpdated := f.Updated == feed.Updated
	if !hasUpdated {
		feed.Updated = f.Updated
	}
	f = *feed

	if hasUpdated && isFeedUpdated {
		c.Infof("feed %s already updated to %v, putting", url, feed.Updated)
		f.Updated = time.Now()
		gn.Put(&f)
		return nil
	}

	c.Debugf("hasUpdate: %v, isFeedUpdated: %v, storyDate: %v", hasUpdated, isFeedUpdated, storyDate)

	var newStories []*Story
	for _, s := range stories {
		if s.Updated.IsZero() || !s.Updated.Before(storyDate) {
			newStories = append(newStories, s)
		}
	}
	c.Debugf("%v possible stories to update", len(newStories))

	puts := []interface{}{&f}

	// find non existant stories
	fk := gn.Key(&f)
	getStories := make([]*Story, len(newStories))
	for i, s := range newStories {
		getStories[i] = &Story{Id: s.Id, Parent: fk}
	}
	err := gn.GetMulti(getStories)
	var updateStories []*Story
	for i, s := range getStories {
		if goon.NotFound(err, i) {
			updateStories = append(updateStories, newStories[i])
		} else if !newStories[i].Updated.IsZero() {
			newStories[i].Created = s.Created
			updateStories = append(updateStories, newStories[i])
		}
	}
	c.Debugf("%v update stories", len(updateStories))

	for _, s := range updateStories {
		puts = append(puts, s,
			&StoryContent{
				Id:      1,
				Parent:  gn.Key(s),
				Content: s.content,
			})
	}
	c.Debugf("putting %v entities", len(puts))

	if !hasUpdated && len(puts) > 1 {
		f.Updated = time.Now()
	}
	if len(puts) > 1 {
		f.Date = time.Now()
	}
	gn.PutMulti(puts)

	return nil
}

func UpdateFeed(c mpg.Context, w http.ResponseWriter, r *http.Request) {
	gn := goon.FromContext(c)
	url := r.FormValue("feed")
	c.Debugf("update feed %s", url)
	f := Feed{Url: url}
	if err := gn.Get(&f); err == datastore.ErrNoSuchEntity {
		return
	}
	if feed, stories := fetchFeed(c, url); feed != nil {
		updateFeed(c, url, feed, stories)
	} else {
		f.NextUpdate = time.Now().Add(time.Hour * 2)
		gn.Put(&f)
		c.Infof("error with %v, bump next update to %v", url, f.NextUpdate)
	}
}