package client

import (
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/simpleiot/simpleiot/data"
)

// Upstream represents an upstream node config
type Upstream struct {
	ID          string `node:"id"`
	Parent      string `node:"parent"`
	Description string `point:"description"`
	URI         string `point:"uri"`
	AuthToken   string `point:"authToken"`
	Disabled    bool   `point:"disabled"`
}

type newEdge struct {
	parent string
	id     string
}

// UpstreamClient is a SIOT client used to handle upstream connections
type UpstreamClient struct {
	nc                  *nats.Conn
	ncLocal             *nats.Conn
	ncRemote            *nats.Conn
	rootLocal           data.NodeEdge
	rootRemote          data.NodeEdge
	config              Upstream
	stop                chan struct{}
	newPoints           chan NewPoints
	newEdgePoints       chan NewPoints
	subRemoteNodePoints map[string]*nats.Subscription
	subRemoteEdgePoints map[string]*nats.Subscription
	chConnected         chan bool
}

// NewUpstreamClient constructor
func NewUpstreamClient(nc *nats.Conn, config Upstream) Client {
	return &UpstreamClient{
		nc:                  nc,
		config:              config,
		stop:                make(chan struct{}),
		newPoints:           make(chan NewPoints),
		newEdgePoints:       make(chan NewPoints),
		chConnected:         make(chan bool),
		subRemoteNodePoints: make(map[string]*nats.Subscription),
		subRemoteEdgePoints: make(map[string]*nats.Subscription),
	}
}

// GetNodes has a 20s timeout, so lets use that here
var syncTimeout = 20 * time.Second

// Start runs the main logic for this client and blocks until stopped
func (up *UpstreamClient) Start() error {
	// create a new NATs connection to the local server as we need to
	// turn echo off
	uri, token, err := GetNatsURI(up.nc)
	if err != nil {
		return fmt.Errorf("Error getting NATS URI: %v", err)
	}

	opts := EdgeOptions{
		URI:       uri,
		AuthToken: token,
		NoEcho:    true,
		Connected: func() {
			log.Println("NATS Local Connected")
		},
		Disconnected: func() {
			log.Println("NATS Local Disconnected")
		},
		Reconnected: func() {
			log.Println("NATS Local Reconnected")
		},
		Closed: func() {
			log.Println("NATS Local Closed")
		},
	}

	up.ncLocal, err = EdgeConnect(opts)
	if err != nil {
		return fmt.Errorf("Error connection to local NATS: %v", err)
	}

	chLocalNodePoints := make(chan NewPoints)
	chLocalEdgePoints := make(chan NewPoints)

	subLocalNodePoints, err := up.ncLocal.Subscribe(SubjectNodeAllPoints(), func(msg *nats.Msg) {
		nodeID, points, err := DecodeNodePointsMsg(msg)

		if err != nil {
			log.Println("Error decoding point: ", err)
			return
		}

		chLocalNodePoints <- NewPoints{ID: nodeID, Points: points}

	})

	subLocalEdgePoints, err := up.ncLocal.Subscribe(SubjectEdgeAllPoints(), func(msg *nats.Msg) {
		nodeID, parentID, points, err := DecodeEdgePointsMsg(msg)

		if err != nil {
			log.Println("Error decoding point: ", err)
			return
		}

		chLocalEdgePoints <- NewPoints{ID: nodeID, Parent: parentID, Points: points}

		for _, p := range points {
			if p.Type == data.PointTypeTombstone && p.Value == 0 {
				// a new node was likely created, make sure we watch it
				err := up.subscribeRemoteNode(parentID, nodeID)
				if err != nil {
					log.Println("Error subscribing to remote node: ", err)
				}
			}
		}
	})

	// FIXME: determine what sync interval we want
	syncTicker := time.NewTicker(time.Second * 10)
	syncTicker.Stop()

	connectTimer := time.NewTimer(time.Millisecond * 10)

	up.rootLocal, err = GetRootNode(up.nc)
	if err != nil {
		return fmt.Errorf("Error getting root node: %v", err)
	}

	chNewEdge := make(chan newEdge)
	var subRemoteUp *nats.Subscription

	connected := false
	initialSub := false

done:
	for {
		select {
		case <-up.stop:
			log.Println("Stopping upstream client: ", up.config.Description)
			break done
		case <-connectTimer.C:
			err := up.connect()
			if err != nil {
				log.Printf("BUG, this should never happen: Error connecting upstream %v: %v\n",
					up.config.Description, err)
				connectTimer.Reset(30 * time.Second)
			}
		case <-syncTicker.C:
			err := up.syncNode("root", up.rootLocal.ID)
			if err != nil {
				log.Println("Error syncing: ", err)
			}

		case conn := <-up.chConnected:
			connected = conn
			if conn {
				if up.rootRemote.ID == "" {
					up.rootRemote, err = GetRootNode(up.ncRemote)
					if err != nil {
						return fmt.Errorf("Error getting upstream root: %v", err)
					}
				}

				if subRemoteUp == nil {
					subject := fmt.Sprintf("up.%v.*.*", up.rootLocal.ID)
					subRemoteUp, err = up.ncRemote.Subscribe(subject, func(msg *nats.Msg) {
						_, id, parent, points, err := DecodeUpEdgePointsMsg(msg)
						if err != nil {
							log.Println("Error decoding remote up points: ", err)
						} else {
							for _, p := range points {
								if p.Type == data.PointTypeTombstone &&
									p.Value == 0 {
									// we have a new node
									chNewEdge <- newEdge{
										parent: parent, id: id}
								}
							}
						}
					})

					if err != nil {
						log.Println("Error subscribing to remote up...: ", err)
					}
				}

				syncTicker.Reset(syncTimeout)
				err := up.syncNode("root", up.rootLocal.ID)
				if err != nil {
					log.Println("Error syncing: ", err)
				}

				if !initialSub {
					// set up initial subscriptions to remote nodes
					err = up.subscribeRemoteNode(up.rootLocal.Parent, up.rootLocal.ID)
					if err != nil {
						log.Println("Upstream: initial sub failed: ", err)
					} else {
						initialSub = true
					}
				}
			} else {
				syncTicker.Stop()
			}
		case pts := <-chLocalNodePoints:
			if connected {
				err = SendNodePoints(up.ncRemote, pts.ID, pts.Points, false)
				if err != nil {
					log.Println("Error sending node points to remote system: ", err)
				}
			}
		case pts := <-chLocalEdgePoints:
			if connected {
				err = SendEdgePoints(up.ncRemote, pts.ID, pts.Parent, pts.Points, false)
				if err != nil {
					log.Println("Error sending edge points to remote system: ", err)
				}
			}
		case pts := <-up.newPoints:
			err := data.MergePoints(pts.ID, pts.Points, &up.config)
			if err != nil {
				log.Println("error merging new points: ", err)
			}

			for _, p := range pts.Points {
				switch p.Type {
				case data.PointTypeURI,
					data.PointTypeAuthToken,
					data.PointTypeDisable:
					// we need to restart the influx write API
					up.disconnect()
					initialSub = false
					err := subRemoteUp.Unsubscribe()
					if err != nil {
						log.Println("subRemoteUp.Unsubscribe() error: ", err)
					}
					subRemoteUp = nil
					connectTimer.Reset(10 * time.Millisecond)
				}
			}

		case pts := <-up.newEdgePoints:
			err := data.MergeEdgePoints(pts.ID, pts.Parent, pts.Points, &up.config)
			if err != nil {
				log.Println("error merging new points: ", err)
			}
		case edge := <-chNewEdge:
		fetchAgain:
			// edge points are sent first, so it may take a bit before we see
			// the node points
			time.Sleep(10 * time.Millisecond)
			nodes, err := GetNodes(up.ncRemote, edge.parent, edge.id, "", true)
			if err != nil {
				log.Println("Error getting node: ", err)
			}
			for _, n := range nodes {
				// if type is not populated yet, try again
				if n.Type == "" {
					goto fetchAgain
				}
				err := up.sendNodesLocal(n)
				if err != nil {
					log.Println("Error chNewEdge sendNodesLocal: ", err)
				}
			}

			err = up.subscribeRemoteNode(edge.parent, edge.id)
			if err != nil {
				log.Println("Error subscribing to new edge: ", err)
			}
		}
	}

	// clean up
	err = subLocalNodePoints.Unsubscribe()
	if err != nil {
		log.Println("Error unsubscribing node points from local bus: ", err)
	}

	err = subLocalEdgePoints.Unsubscribe()
	if err != nil {
		log.Println("Error unsubscribing edge points from local bus: ", err)
	}

	if subRemoteUp != nil {
		err = subRemoteUp.Unsubscribe()
		if err != nil {
			log.Println("Error unsubscribingfrom subRemoteUp: ", err)
		}
	}

	up.disconnect()
	up.ncLocal.Close()

	return nil
}

// Stop sends a signal to the Start function to exit
func (up *UpstreamClient) Stop(err error) {
	close(up.stop)
}

// Points is called by the Manager when new points for this
// node are received.
func (up *UpstreamClient) Points(nodeID string, points []data.Point) {
	up.newPoints <- NewPoints{nodeID, "", points}
}

// EdgePoints is called by the Manager when new edge points for this
// node are received.
func (up *UpstreamClient) EdgePoints(nodeID, parentID string, points []data.Point) {
	up.newEdgePoints <- NewPoints{nodeID, parentID, points}
}

func (up *UpstreamClient) connect() error {
	if up.config.Disabled {
		log.Printf("Upstream %v disabled", up.config.Description)
		return nil
	}

	opts := EdgeOptions{
		URI:       up.config.URI,
		AuthToken: up.config.AuthToken,
		NoEcho:    true,
		Connected: func() {
			up.chConnected <- true
			log.Println("NATS Upstream Connected")
		},
		Disconnected: func() {
			up.chConnected <- false
			log.Println("NATS Upstream Disconnected")
		},
		Reconnected: func() {
			up.chConnected <- true
			log.Println("NATS Upstream Reconnected")
		},
		Closed: func() {
			log.Println("NATS Upstream Closed")
		},
	}

	var err error
	up.ncRemote, err = EdgeConnect(opts)

	if err != nil {
		return fmt.Errorf("Error connection to upstream NATS: %v", err)
	}

	return nil
}

func (up *UpstreamClient) subscribeRemoteNodePoints(id string) error {
	if _, ok := up.subRemoteNodePoints[id]; !ok {
		var err error
		up.subRemoteNodePoints[id], err = up.ncRemote.Subscribe(SubjectNodePoints(id), func(msg *nats.Msg) {
			nodeID, points, err := DecodeNodePointsMsg(msg)
			if err != nil {
				log.Println("Error decoding point: ", err)
				return
			}

			err = SendNodePoints(up.ncLocal, nodeID, points, false)
			if err != nil {
				log.Println("Error sending node points to remote system: ", err)
			}
		})

		if err != nil {
			return err
		}
	}

	return nil
}

func (up *UpstreamClient) subscribeRemoteEdgePoints(parent, id string) error {
	if _, ok := up.subRemoteEdgePoints[id]; !ok {
		var err error
		key := id + ":" + parent
		up.subRemoteEdgePoints[key], err = up.ncRemote.Subscribe(SubjectEdgePoints(id, parent),
			func(msg *nats.Msg) {
				nodeID, parentID, points, err := DecodeEdgePointsMsg(msg)
				if err != nil {
					log.Println("Error decoding point: ", err)
					return
				}

				err = SendEdgePoints(up.ncLocal, nodeID, parentID, points, false)
				if err != nil {
					log.Println("Error sending edge points to remote system: ", err)
				}
			})

		if err != nil {
			return err
		}
	}
	return nil
}

func (up *UpstreamClient) subscribeRemoteNode(parent, id string) error {
	err := up.subscribeRemoteNodePoints(id)
	if err != nil {
		return err
	}

	err = up.subscribeRemoteEdgePoints(parent, id)
	if err != nil {
		return err
	}

	// we walk through all local nodes and and subscribe to remote changes
	children, err := GetNodes(up.ncLocal, id, "all", "", true)
	if err != nil {
		return err
	}

	for _, c := range children {
		err := up.subscribeRemoteNode(c.Parent, c.ID)
		if err != nil {
			return err
		}
	}

	return nil
}

func (up *UpstreamClient) disconnect() {
	for key, sub := range up.subRemoteNodePoints {
		err := sub.Unsubscribe()
		if err != nil {
			log.Println("Error unsubscribing from remote: ", err)
		}
		delete(up.subRemoteNodePoints, key)
	}

	for key, sub := range up.subRemoteEdgePoints {
		err := sub.Unsubscribe()
		if err != nil {
			log.Println("Error unsubscribing from remote: ", err)
		}
		delete(up.subRemoteEdgePoints, key)
	}

	if up.ncRemote != nil {
		up.ncRemote.Close()
		up.ncRemote = nil
	}
}

// sendNodesRemote is used to send node and children over nats
// from one NATS server to another. Typically from the current instance
// to an upstream.
func (up *UpstreamClient) sendNodesRemote(node data.NodeEdge) error {

	if node.Parent == "root" {
		node.Parent = up.rootRemote.ID
	}

	err := SendNode(up.ncRemote, node, up.config.ID)
	if err != nil {
		return err
	}

	// process child nodes
	childNodes, err := GetNodes(up.nc, node.ID, "all", "", false)
	if err != nil {
		return fmt.Errorf("Error getting node children: %v", err)
	}

	for _, childNode := range childNodes {
		err := up.sendNodesRemote(childNode)

		if err != nil {
			return fmt.Errorf("Error sending child node: %v", err)
		}
	}

	return nil
}

// sendNodesLocal is used to send node and children over nats
// from one NATS server to another. Typically from the current instance
// to an upstream.
func (up *UpstreamClient) sendNodesLocal(node data.NodeEdge) error {
	err := SendNode(up.ncLocal, node, up.config.ID)
	if err != nil {
		return err
	}

	// process child nodes
	childNodes, err := GetNodes(up.nc, node.ID, "all", "", false)
	if err != nil {
		return fmt.Errorf("Error getting node children: %v", err)
	}

	for _, childNode := range childNodes {
		err := up.sendNodesLocal(childNode)

		if err != nil {
			return fmt.Errorf("Error sending child node: %v", err)
		}
	}

	return nil
}

func (up *UpstreamClient) syncNode(parent, id string) error {
	if parent == "root" {
		parent = "all"
	}

	nodeLocals, err := GetNodes(up.nc, parent, id, "", true)
	if err != nil {
		return fmt.Errorf("Error getting local node: %v", err)
	}

	if len(nodeLocals) == 0 {
		return errors.New("local nodes not found")
	}

	nodeLocal := nodeLocals[0]

	nodeUps, upErr := GetNodes(up.ncRemote, parent, id, "", true)
	if upErr != nil {
		if upErr != data.ErrDocumentNotFound {
			return fmt.Errorf("Error getting upstream root node: %v", upErr)
		}
	}

	var nodeUp data.NodeEdge

	if len(nodeUps) == 0 || upErr == data.ErrDocumentNotFound {
		log.Printf("Upstream node %v does not exist, sending\n", nodeLocal.Desc())
		err := up.sendNodesRemote(nodeLocal)
		if err != nil {
			return fmt.Errorf("Error sending node upstream: %w", err)
		}

		err = up.subscribeRemoteNode(nodeLocal.ID, nodeLocal.Parent)
		if err != nil {
			return fmt.Errorf("Error subscribing to node changes: %w", err)
		}

		return nil
	}

	nodeUp = nodeUps[0]

	if nodeUp.Hash != nodeLocal.Hash {
		log.Printf("sync %v: syncing node: %v, hash up: 0x%x, down: 0x%x ",
			up.config.Description,
			nodeLocal.Desc(),
			nodeUp.Hash, nodeLocal.Hash)

		// first compare node points
		// key in below map is the index of the point in the upstream node
		upstreamProcessed := make(map[int]bool)

		for _, p := range nodeLocal.Points {
			found := false
			for i, pUp := range nodeUp.Points {
				if p.IsMatch(pUp.Type, pUp.Key) {
					found = true
					upstreamProcessed[i] = true
					if p.Time.After(pUp.Time) {
						// need to send point upstream
						err := SendNodePoint(up.ncRemote, nodeUp.ID, p, true)
						if err != nil {
							log.Println("Error syncing point upstream: ", err)
						}
					} else if p.Time.Before(pUp.Time) {
						// need to update point locally
						err := SendNodePoint(up.nc, nodeLocal.ID, pUp, true)
						if err != nil {
							log.Println("Error syncing point from upstream: ", err)
						}
					}
				}
			}

			if !found {
				SendNodePoint(up.ncRemote, nodeUp.ID, p, true)
			}
		}

		// check for any points that do not exist locally
		for i, pUp := range nodeUp.Points {
			if _, ok := upstreamProcessed[i]; !ok {
				err := SendNodePoint(up.nc, nodeLocal.ID, pUp, true)
				if err != nil {
					log.Println("Error syncing point from upstream: ", err)
				}
			}
		}

		// now compare edge points
		// key in below map is the index of the point in the upstream node
		upstreamProcessed = make(map[int]bool)

		for _, p := range nodeLocal.EdgePoints {
			found := false
			for i, pUp := range nodeUp.EdgePoints {
				if p.IsMatch(pUp.Type, pUp.Key) {
					found = true
					upstreamProcessed[i] = true
					if p.Time.After(pUp.Time) {
						// need to send point upstream
						err := SendEdgePoint(up.ncRemote, nodeUp.ID, nodeUp.Parent, p, true)
						if err != nil {
							log.Println("Error syncing point upstream: ", err)
						}
					} else if p.Time.Before(pUp.Time) {
						// need to update point locally
						err := SendEdgePoint(up.nc, nodeLocal.ID, nodeLocal.Parent, pUp, true)
						if err != nil {
							log.Println("Error syncing point from upstream: ", err)
						}
					}
				}
			}

			if !found {
				SendEdgePoint(up.ncRemote, nodeUp.ID, nodeUp.Parent, p, true)
			}
		}

		// check for any points that do not exist locally
		for i, pUp := range nodeUp.EdgePoints {
			if _, ok := upstreamProcessed[i]; !ok {
				err := SendEdgePoint(up.nc, nodeLocal.ID, nodeLocal.Parent, pUp, true)
				if err != nil {
					log.Println("Error syncing edge point from upstream: ", err)
				}
			}
		}

		// sync child nodes
		children, err := GetNodes(up.nc, nodeLocal.ID, "all", "", false)
		if err != nil {
			return fmt.Errorf("Error getting local node children: %v", err)
		}

		// FIXME optimization we get the edges here and not the full child node
		upChildren, err := GetNodes(up.ncRemote, nodeUp.ID, "all", "", false)
		if err != nil {
			return fmt.Errorf("Error getting upstream node children: %v", err)
		}

		// map index is index of upChildren
		upChildProcessed := make(map[int]bool)

		for _, child := range children {
			found := false
			for i, upChild := range upChildren {
				if child.ID == upChild.ID {
					found = true
					upChildProcessed[i] = true
					if child.Hash != upChild.Hash {
						err := up.syncNode(nodeLocal.ID, child.ID)
						if err != nil {
							fmt.Println("Error syncing node: ", err)
						}
					}
				}
			}

			if !found {
				// need to send node upstream
				err := up.sendNodesRemote(child)
				if err != nil {
					log.Println("Error sending node upstream: ", err)
				}

				err = up.subscribeRemoteNode(child.Parent, child.ID)
				if err != nil {
					log.Println("Error subscribing to upstream: ", err)
				}
			}
		}
		for i, upChild := range upChildren {
			if _, ok := upChildProcessed[i]; !ok {
				err := up.sendNodesLocal(upChild)
				if err != nil {
					log.Println("Error getting node from upstream: ", err)
				}
				err = up.subscribeRemoteNode(upChild.Parent, upChild.ID)
				if err != nil {
					log.Println("Error subscribing to upstream: ", err)
				}
			}
		}
	}

	return nil
}
