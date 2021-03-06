package kafkazk

import (
	"fmt"
	"math/rand"
	"sort"
)

// BrokerMetaMap is a map of broker IDs to BrokerMeta
// metadata fetched from ZooKeeper. Currently, just
// the rack field is retrieved.
type BrokerMetaMap map[int]*BrokerMeta

// BrokerMeta holds metadata that describes a broker,
// used in satisfying constraints.
type BrokerMeta struct {
	StorageFree       float64 // In bytes.
	MetricsIncomplete bool
	// Metadata from ZooKeeper.
	ListenerSecurityProtocolMap map[string]string `json:"listener_security_protocol_map"`
	Endpoints                   []string          `json:"endpoints"`
	Rack                        string            `json:"rack"`
	JMXPort                     int               `json:"jmx_port"`
	Host                        string            `json:"host"`
	Timestamp                   string            `json:"timestamp"`
	Port                        int               `json:"port"`
	Version                     int               `json:"version"`
}

// BrokerMetricsMap holds a mapping of broker
// ID to BrokerMetrics.
type BrokerMetricsMap map[int]*BrokerMetrics

// BrokerMetrics holds broker metric
// data fetched from ZK.
type BrokerMetrics struct {
	StorageFree float64
}

// BrokerUseStats holds counts
// of partition ownership.
type BrokerUseStats struct {
	ID       int
	Leader   int
	Follower int
}

// BrokerUseStatsList is a slice of *BrokerUseStats.
type BrokerUseStatsList []*BrokerUseStats

func (b BrokerUseStatsList) Len() int           { return len(b) }
func (b BrokerUseStatsList) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b BrokerUseStatsList) Less(i, j int) bool { return b[i].ID < b[j].ID }

// BrokerStatus summarizes change counts
// from an input and output broker list.
type BrokerStatus struct {
	New        int
	Missing    int
	OldMissing int
	Replace    int
}

// Changes returns a bool that indicates whether a
// BrokerStatus values represent a change in brokers.
func (bs BrokerStatus) Changes() bool {
	switch {
	case bs.New != 0, bs.Missing != 0, bs.OldMissing != 0, bs.Replace != 0:
		return true
	}

	return false
}

// Broker associates metadata with a real broker by ID.
type Broker struct {
	ID          int
	Locality    string
	Used        int
	StorageFree float64
	Replace     bool
	Missing     bool
	New         bool
}

// BrokerMap holds a mapping of broker IDs to *Broker.
type BrokerMap map[int]*Broker

// BrokerList is a slice of brokers for sorting by used count.
type BrokerList []*Broker

// Wrapper types for sort by methods.
type brokersByCount BrokerList
type brokersByStorage BrokerList
type brokersByID BrokerList

// Satisfy the sort interface for BrokerList types.

// By used field value.
func (b brokersByCount) Len() int      { return len(b) }
func (b brokersByCount) Swap(i, j int) { b[i], b[j] = b[j], b[i] }
func (b brokersByCount) Less(i, j int) bool {
	if b[i].Used < b[j].Used {
		return true
	}
	if b[i].Used > b[j].Used {
		return false
	}

	return b[i].ID < b[j].ID
}

// By StorageFree value.
func (b brokersByStorage) Len() int      { return len(b) }
func (b brokersByStorage) Swap(i, j int) { b[i], b[j] = b[j], b[i] }
func (b brokersByStorage) Less(i, j int) bool {
	if b[i].StorageFree > b[j].StorageFree {
		return true
	}
	if b[i].StorageFree < b[j].StorageFree {
		return false
	}

	return b[i].ID < b[j].ID
}

// By ID value ascending.
func (b brokersByID) Len() int           { return len(b) }
func (b brokersByID) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b brokersByID) Less(i, j int) bool { return b[i].ID < b[j].ID }

// Sort methods.

// SortByCount sorts the BrokerList by Used values.
func (b BrokerList) SortByCount() {
	sort.Sort(brokersByCount(b))
}

// SortByStorage sorts the BrokerList by StorageFree values.
func (b BrokerList) SortByStorage() {
	sort.Sort(brokersByStorage(b))
}

// SortByID sorts the BrokerList by ID values.
func (b BrokerList) SortByID() {
	sort.Sort(brokersByID(b))
}

// SortPseudoShuffle takes a BrokerList and performs a sort by count.
// For each sequence of brokers with equal counts, the sub-slice is
// pseudo random shuffled using the provided seed value s.
func (b BrokerList) SortPseudoShuffle(seed int64) {
	sort.Sort(brokersByCount(b))

	if len(b) <= 2 {
		return
	}

	rand.Seed(seed)

	s := 0
	stop := len(b) - 1
	currVal := b[0].Used

	// For each continuous run of
	// a given Used value, shuffle
	// that range of the slice.
	for k := range b {
		switch {
		case b[k].Used != currVal:
			currVal = b[k].Used
			rand.Shuffle(len(b[s:k]), func(i, j int) {
				b[s:k][i], b[s:k][j] = b[s:k][j], b[s:k][i]
			})
			s = k
		case k == stop:
			rand.Shuffle(len(b[s:]), func(i, j int) {
				b[s:][i], b[s:][j] = b[s:][j], b[s:][i]
			})
		}
	}
}

// Update takes a []int of broker IDs and BrokerMap then adds them to the
// BrokerMap, returning the count of marked for replacement, newly included,
// and brokers that weren't found in ZooKeeper. Additionally, a channel
// of msgs describing changes is returned.
func (b BrokerMap) Update(bl []int, bm BrokerMetaMap) (*BrokerStatus, <-chan string) {
	bs := &BrokerStatus{}
	msgs := make(chan string, len(b)+(len(bl)*3))

	// Build a map from the new broker list.
	newBrokers := map[int]bool{}
	for _, broker := range bl {
		newBrokers[broker] = true
	}

	// Do an initial pass on existing brokers
	// and see if any are missing in ZooKeeper.
	if len(bm) > 0 {
		for id := range b {
			// Skip reserved ID 0.
			if id == 0 {
				continue
			}

			if _, exist := bm[id]; !exist {
				msgs <- fmt.Sprintf("Previous broker %d missing", id)
				b[id].Replace = true
				b[id].Missing = true
				// If this broker is missing and was provided in
				// the broker list, consider it a "missing provided broker".
				if _, ok := newBrokers[id]; len(bm) > 0 && ok {
					bs.Missing++
				} else {
					bs.OldMissing++
				}
			}
		}
	}

	// Set the replace flag for existing brokers
	// not in the new broker map.
	for _, broker := range b {
		// Broker ID 0 is a special stub
		// ID used for internal purposes.
		// Skip it.
		if broker.ID == 0 {
			continue
		}

		if _, ok := newBrokers[broker.ID]; !ok {
			bs.Replace++
			b[broker.ID].Replace = true
			msgs <- fmt.Sprintf("Broker %d marked for removal", broker.ID)
		}
	}

	// Merge new brokers with existing brokers.
	for id := range newBrokers {
		// Don't overwrite existing (which will be most brokers).
		if b[id] == nil {
			// Skip metadata lookups if
			// meta is not being used.
			if len(bm) == 0 {
				b[id] = &Broker{
					Used:    0,
					ID:      id,
					Replace: false,
					New:     true,
				}
				bs.New++
				continue
			}

			// Else check the broker against
			// the broker metadata map.
			if meta, exists := bm[id]; exists {
				b[id] = &Broker{
					Used:        0,
					ID:          id,
					Replace:     false,
					Locality:    meta.Rack,
					StorageFree: meta.StorageFree,
					New:         true,
				}
				bs.New++
			} else {
				bs.Missing++
				msgs <- fmt.Sprintf("Broker %d not found in ZooKeeper", id)
			}
		}
	}

	// Log new brokers.
	for _, broker := range b {
		if broker.New {
			msgs <- fmt.Sprintf("New broker %d", broker.ID)
		}
	}

	close(msgs)

	return bs, msgs
}

// SubStorageAll takes a PartitionMap, PartitionMetaMap, and a function. For all
// brokers that return true as an input to function f, the size of all partitions
// held is added back to the broker StorageFree value.
func (b BrokerMap) SubStorage(pm *PartitionMap, pmm PartitionMetaMap, f func(*Broker) bool) error {
	// Get the size of each partition.
	for _, partn := range pm.Partitions {
		size, err := pmm.Size(partn)
		if err != nil {
			return err
		}

		// Add this size back to the
		// StorageFree for all mapped brokers.
		for _, bid := range partn.Replicas {
			if broker, exists := b[bid]; exists {
				if f(broker) {
					broker.StorageFree += size
				}
			} else {
				return fmt.Errorf("Broker %d not found in broker map", bid)
			}
		}
	}

	return nil
}

// Filter returns a BrokerMap of brokers that return
// true as an input to function f.
func (b BrokerMap) Filter(f func(*Broker) bool) BrokerMap {
	bmap := BrokerMap{}

	for _, broker := range b {
		if broker.ID == 0 {
			continue
		}

		if f(broker) {
			bmap[broker.ID] = broker
		}
	}

	return bmap
}

// List take a BrokerMap and returns a BrokerList.
func (b BrokerMap) List() BrokerList {
	bl := BrokerList{}

	for broker := range b {
		bl = append(bl, b[broker])
	}

	return bl
}

// BrokerMapFromPartitionMap creates a BrokerMap from a partitionMap.
func BrokerMapFromPartitionMap(pm *PartitionMap, bm BrokerMetaMap, force bool) BrokerMap {
	bmap := BrokerMap{}
	// For each partition.
	for _, partition := range pm.Partitions {
		// For each broker in the
		// partition replica set.
		for _, id := range partition.Replicas {
			// If the broker isn't in the
			// broker map, add it.
			if bmap[id] == nil {
				// If we're doing a force rebuid, replace
				// should be set to true.
				bmap[id] = &Broker{Used: 0, ID: id, Replace: false}
			}

			// Track use scoring unless we're
			// doing a force rebuild. In this case,
			// we're treating existing brokers the same
			// as new brokers (which start with a score of 0).
			if !force {
				bmap[id].Used++
			}

			// Add metadata if we have it.
			if meta, exists := bm[id]; exists {
				bmap[id].Locality = meta.Rack
				bmap[id].StorageFree = meta.StorageFree
			}
		}
	}

	// Broker ID 0 is used for --force-rebuild.
	// We request a Stripped map which replaces
	// all existing brokers with the fake broker
	// with ID set for replacement.
	bmap[0] = &Broker{Used: 0, ID: 0, Replace: true}

	return bmap
}

// Copy returns a copy of a BrokerMap.
func (b BrokerMap) Copy() BrokerMap {
	c := BrokerMap{}
	for id, br := range b {
		c[id] = &Broker{
			ID:          br.ID,
			Locality:    br.Locality,
			Used:        br.Used,
			StorageFree: br.StorageFree,
			Replace:     br.Replace,
			Missing:     br.Missing,
			New:         br.New,
		}
	}

	return c
}

// Copy returns a copy of a Broker.
func (b Broker) Copy() Broker {
	return Broker{
		ID:          b.ID,
		Locality:    b.Locality,
		Used:        b.Used,
		StorageFree: b.StorageFree,
		Replace:     b.Replace,
		Missing:     b.Missing,
		New:         b.New,
	}
}
