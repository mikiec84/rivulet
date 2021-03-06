package main

import (
	"crypto/md5"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"
	"math/rand"
)

import (
	"appengine"
	"appengine/datastore"
	"appengine/delay"
	"appengine/memcache"
	"appengine/urlfetch"
	"appengine/user"
)

const defaultRefreshDelay = time.Minute * 30

var refreshSubscriptionURLDelay = delay.Func("refresh", refreshSubscriptionURL)
var refreshDelay = delay.Func("refresh", func(context appengine.Context, x string) { refresh(context, x != "false") })

var LengthUnequal = errors.New("couldn't make md5 hash")

func refreshSubscription(context appengine.Context, feed Feed, feedkey *datastore.Key) (err error) {
	if feed.URL == "" {
		return
	}

	now := time.Now()

	var subscription SubscriptionCache
	var item *memcache.Item
	item, err = memcache.Gob.Get(context, feed.URL, &subscription)
	if err == memcache.ErrCacheMiss {
		subscription.URL = feed.URL
		item = &memcache.Item{Key: feed.URL}
		err = nil
	} else if err != nil {
		return
	}

	if now.Unix() > subscription.Update {
		duration := time.Minute * time.Duration(rand.Intn(60))

		client := urlfetch.Client(context)
		printInfo(context, fmt.Sprintf("fetching %v", feed.URL))
		var response *http.Response
		response, err = client.Get(feed.URL)
		if err != nil {
			return
		}
		defer response.Body.Close()

		var body []byte
		body, err = ioutil.ReadAll(response.Body)
		if err != nil {
			return
		}

		if len(body) != subscription.Length {
			subscription.Length = len(body)
			hash := md5.New()
			hashLength := 0
			hashLength, err = hash.Write(body)
			if err != nil {
				return
			}
			if hashLength != len(body) {
				return LengthUnequal
			}

			sum := fmt.Sprintf("%x", hash.Sum(nil))
			if sum != subscription.MD5 {
				if subscription.Format == UNKNOWN {
					subscription.Format = getFeedType(response, body)
				}
				/*if subscription.Format == HTMLFeed {
					var html HTML
					err = xml.Unmarshal(body, &html)
					if err != nil {
						subscription.Format = OTHER
					}
					for _, link := range html.Head.Links {
						if link.Rel == "alternate" {
							subscription.URL = 
							break
						}
					}
				}*/
				if subscription.Format == OTHER {
					err = memcache.Delete(context, feed.URL)
					if err != nil && err != memcache.ErrCacheMiss {
						return
					}
					return errors.New(fmt.Sprintf("%v is not a feed, deleted", feed.URL))
				}

				var feedCache FeedCache
				feedCache, err = getSubscription(context, subscription.Format, body, feed.URL)
				if err != nil {
					return
				}

				if feedCache.TimeToLive > 0 {
					duration = time.Duration(feedCache.TimeToLive)
				}

				for _, article := range feedCache.Articles {
					if !ContainsString(feed.Articles, article.ID) {
						feed.Articles = append(feed.Articles, article.ID)
						err = addArticle(context, feed, article)
						if err != nil {
							printError(context, err, article.ID)
							err = nil
							continue
						}
					}
				}

				_, err = datastore.Put(context, feedkey, &feed)
				subscription.MD5 = sum
			}
		}

		subscription.Update = now.Add(duration).Unix()
		item.Object = subscription
		err = memcache.Gob.Set(context, item)
	}

	return
}

func refreshSubscriptionURL(context appengine.Context, url string) (err error) {
	var feed Feed
	var feedkey *datastore.Key
	feedkey, err = GetFirst(context, "Feed", "URL", url, &feed)
	if err == datastore.Done {
		feed.URL = url
		feedkey = datastore.NewIncompleteKey(context, "Feed", nil)
	} else if err != nil {
		return
	}

	return refreshSubscription(context, feed, feedkey)
}

func refresh(context appengine.Context, asNeeded bool) (data Data, err error) {
	query := datastore.NewQuery("Feed")
	var keys []*datastore.Key
	keys, err = query.KeysOnly().GetAll(context, nil)

	for _, key := range keys {
		var feed Feed
		err = datastore.Get(context, key, &feed)
		if err == datastore.Done {
			break
		} else if err != nil {
			printError(context, err, feed.URL)
			err = nil
			continue
		}

		if asNeeded {
			err = refreshSubscription(context, feed, key)
		} else {
			_, err = getSubscriptionURL(context, feed.URL)
		}
		if err != nil {
			printError(context, err, feed.URL)
			err = nil
			continue
		}
	}

	return
}

func refreshGET(context appengine.Context, user *user.User, request *http.Request) (data Data, err error) {
	url := request.FormValue("url")
	if url != "" {
		return nil, refreshSubscriptionURL(context, url)
	}

	asNeeded := true
	force := request.FormValue("force")
	if force != "" {
		asNeeded = false
	}

	delay := request.FormValue("delay")
	if delay != "" {
		if asNeeded {
			refreshDelay.Call(context, "true")
		} else {
			refreshDelay.Call(context, "false")
		}
		return
	}

	refresh(context, asNeeded)

	return
}
