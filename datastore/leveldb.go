package datastore

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/cache"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"github.com/vmihailenco/msgpack"
	"io/ioutil"
	"os"
	"strconv"
)

const (
	STORE_LEVELDB = "leveldb"

	prefixUsers   = "users"
	prefixServers = "servers"

	LEVEL_ENCODER_GOB    = "gob"
	LEVEL_ENCODER_MSGPAK = "msgpack"
)

func init() {
	RegisterStore(STORE_LEVELDB, NewLevelDataStore)
}

type marshaller interface {
	Marshal(v interface{}) ([]byte, error)
	Unmarshal(data []byte, v interface{}) error
}

type msgpackEncoder struct{}
type gobEncoder struct{}

func (m msgpackEncoder) Marshal(v interface{}) ([]byte, error) {
	return msgpack.Marshal(v)
}

func (m msgpackEncoder) Unmarshal(data []byte, v interface{}) error {
	return msgpack.Unmarshal(data, v)
}

func (g gobEncoder) Marshal(v interface{}) ([]byte, error) {
	m := new(bytes.Buffer)
	enc := gob.NewEncoder(m)
	enc.Encode(v)
	return m.Bytes(), nil
}

func (g gobEncoder) Unmarshal(data []byte, v interface{}) error {
	m := bytes.NewBuffer(data)
	dec := gob.NewDecoder(m)
	return dec.Decode(v)
}

type LevelDataStore struct {
	db      *leveldb.DB
	dbPath  string
	encoder marshaller
	isTemp  bool
}

func NewLevelDataStore(conf map[string]string) (DataStore, error) {
	encoder := LEVEL_ENCODER_MSGPAK
	path := ""
	writeBuffer := 16777216
	readCache := 16777216
	if v, ok := conf["debug_leveldb_encoder"]; ok && v != "" {
		switch v {
		case LEVEL_ENCODER_GOB, LEVEL_ENCODER_MSGPAK:
			encoder = v
		}
	}
	if v, ok := conf["leveldb_path"]; ok && v != "" {
		path = v
	}
	if v, ok := conf["leveldb_cache"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 8388608 {
				n = 8388608
			}
			readCache = n
		}
	}
	if v, ok := conf["leveldb_writebuffer"]; ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			if n < 4194304 {
				n = 4194304
			}
			writeBuffer = n
		}
	}
	o := &opt.Options{
		Filter:      filter.NewBloomFilter(10),
		WriteBuffer: writeBuffer,
		BlockCache:  cache.NewLRUCache(readCache),
	}
	lds := new(LevelDataStore)
	lds.dbPath = path
	if lds.dbPath == "" {
		if p, err := ioutil.TempDir("", "tblvl"); err == nil {
			lds.dbPath = p
			lds.isTemp = true
		} else {
			return nil, err
		}
	}
	switch encoder {
	case LEVEL_ENCODER_MSGPAK:
		lds.encoder = msgpackEncoder{}
	default:
		lds.encoder = gobEncoder{}
	}
	if db, err := leveldb.OpenFile(lds.dbPath, o); err == nil {
		lds.db = db
		return lds, nil
	} else {
		return nil, err
	}
}

func serverKey(s Server) []byte {
	return []byte(fmt.Sprintf("%s%s:%d", prefixServers, s.Hostname, s.Port))
}

func (u *User) secondaryKey(x int64) []byte {
	b := make([]byte, 8+idLength)
	binary.BigEndian.PutUint64(b, uint64(x))
	copy(b[8:], u.Id)
	return b
}

func (u *User) secondaryKeyReverse(x int64) []byte {
	return u.secondaryKey(^x)
}

func (lds *LevelDataStore) GetUsers(sort string, limit int, skip int) ([]*User, error) {
	capc := limit
	if capc == 0 {
		capc = 64
	}
	users := make([]*User, 0, capc)
	prefixS := prefixUsers
	switch sort {
	case SORT_KILLS:
		prefixS += ":bk"
	case SORT_DEATHS:
		prefixS += ":bd"
	case SORT_SCORE:
		prefixS += ":bs"
	case SORT_WINS:
		prefixS += ":bw"
	case SORT_LOSSES:
		prefixS += ":bl"
	case SORT_PLAYS:
		prefixS += ":bp"
	default:
		prefixS += ":bu"
	}
	prefix := []byte(prefixS)
	iter := lds.db.NewIterator(nil)
	defer iter.Release()
	for i, hasNext := 0, iter.Seek(prefix); hasNext && bytes.HasPrefix(iter.Key(), prefix) && (limit == 0 || i < limit); i, hasNext = i+1, iter.Next() {
		if skip > 0 {
			skip--
			i--
			continue
		}
		u := new(User)
		lds.encoder.Unmarshal(iter.Value(), u)
		users = append(users, u)
	}
	return users, nil
}

func (lds *LevelDataStore) GetUsersAdjacent(user *User, sort string, spread int) ([]*User, error) {
	capc := spread * 2
	if capc == 0 {
		capc = 64
	}
	users := make([]*User, 0, capc)
	prefixS := prefixUsers
	var value []byte
	switch sort {
	case SORT_KILLS:
		prefixS += ":bk"
		value = user.secondaryKeyReverse(user.Kills)
	case SORT_DEATHS:
		prefixS += ":bd"
		value = user.secondaryKeyReverse(user.Deaths)
	case SORT_SCORE:
		prefixS += ":bs"
		value = user.secondaryKeyReverse(user.Score)
	case SORT_WINS:
		prefixS += ":bw"
		value = user.secondaryKeyReverse(user.Wins)
	case SORT_LOSSES:
		prefixS += ":bl"
		value = user.secondaryKeyReverse(user.Losses)
	case SORT_PLAYS:
		prefixS += ":bp"
		value = user.secondaryKeyReverse(user.Plays)
	default:
		prefixS += ":bu"
		value = []byte(user.Name)
	}
	hPrefix := []byte(prefixS)
	prefix := append(hPrefix[:], value...)

	iter := lds.db.NewIterator(nil)
	defer iter.Release()

	iter.Seek(prefix)
	for i, hasPrev := 0, iter.Prev(); hasPrev && bytes.HasPrefix(iter.Key(), hPrefix) && (spread == -1 || i < spread); i, hasPrev = i+1, iter.Prev() {
		u := new(User)
		lds.encoder.Unmarshal(iter.Value(), u)
		users = append(users, u)
	}
	uq := make([]*User, len(users), cap(users))
	for idx, _ := range users {
		uq[idx] = users[len(users)-idx-1]
	}
	users = uq

	for i, hasNext := 0, iter.Seek(prefix); hasNext && bytes.HasPrefix(iter.Key(), hPrefix) && (spread == -1 || i <= spread); i, hasNext = i+1, iter.Next() {
		u := new(User)
		lds.encoder.Unmarshal(iter.Value(), u)
		users = append(users, u)
	}

	return users, nil
}

func (lds *LevelDataStore) GetUser(username string) (*User, error) {
	k := []byte(prefixUsers + ":bu" + username)
	if value, err := lds.db.Get(k, nil); err == nil {
		u := new(User)
		lds.encoder.Unmarshal(value, u)
		return u, nil
	} else if err == util.ErrNotFound {
		return nil, ErrUserNotFound
	} else {
		return nil, err
	}
}

func (lds *LevelDataStore) PutUser(u *User) error {
	if v, e := lds.encoder.Marshal(u); e == nil {
		if e := lds.updateUser(u); e == nil {
			batch := new(leveldb.Batch)

			batch.Put(append([]byte(prefixUsers+":bk"), u.secondaryKeyReverse(u.Kills)...), v)
			batch.Put(append([]byte(prefixUsers+":bd"), u.secondaryKeyReverse(u.Deaths)...), v)
			batch.Put(append([]byte(prefixUsers+":bs"), u.secondaryKeyReverse(u.Score)...), v)
			batch.Put(append([]byte(prefixUsers+":bw"), u.secondaryKeyReverse(u.Wins)...), v)
			batch.Put(append([]byte(prefixUsers+":bl"), u.secondaryKeyReverse(u.Losses)...), v)
			batch.Put(append([]byte(prefixUsers+":bp"), u.secondaryKeyReverse(u.Plays)...), v)
			batch.Put(append([]byte(prefixUsers+":bu"), []byte(u.Name)...), v)

			return lds.db.Write(batch, nil)
		} else {
			return e
		}
	} else {
		return e
	}
}

func (lds *LevelDataStore) updateUser(newUser *User) error {
	if oldUser, err := lds.GetUser(newUser.Name); err == nil {
		batch := new(leveldb.Batch)

		if oldUser.Kills != newUser.Kills {
			batch.Delete(append([]byte(prefixUsers+":bk"), oldUser.secondaryKeyReverse(oldUser.Kills)...))
		}
		if oldUser.Deaths != newUser.Deaths {
			batch.Delete(append([]byte(prefixUsers+":bd"), oldUser.secondaryKeyReverse(oldUser.Deaths)...))
		}
		if oldUser.Score != newUser.Score {
			batch.Delete(append([]byte(prefixUsers+":bs"), oldUser.secondaryKeyReverse(oldUser.Score)...))
		}
		if oldUser.Wins != newUser.Wins {
			batch.Delete(append([]byte(prefixUsers+":bw"), oldUser.secondaryKeyReverse(oldUser.Wins)...))
		}
		if oldUser.Losses != newUser.Losses {
			batch.Delete(append([]byte(prefixUsers+":bl"), oldUser.secondaryKeyReverse(oldUser.Losses)...))
		}
		if oldUser.Plays != newUser.Plays {
			batch.Delete(append([]byte(prefixUsers+":bp"), oldUser.secondaryKeyReverse(oldUser.Plays)...))
		}
		if oldUser.Name != newUser.Name {
			batch.Delete(append([]byte(prefixUsers+":bu"), []byte(oldUser.Name)...))
		}

		return lds.db.Write(batch, nil)
	} else if err == ErrUserNotFound {
		return nil
	} else {
		return err
	}
}

func (lds *LevelDataStore) DeleteUser(u *User) error {
	batch := new(leveldb.Batch)

	batch.Delete(append([]byte(prefixUsers+":bk"), u.secondaryKeyReverse(u.Kills)...))
	batch.Delete(append([]byte(prefixUsers+":bd"), u.secondaryKeyReverse(u.Deaths)...))
	batch.Delete(append([]byte(prefixUsers+":bs"), u.secondaryKeyReverse(u.Score)...))
	batch.Delete(append([]byte(prefixUsers+":bw"), u.secondaryKeyReverse(u.Wins)...))
	batch.Delete(append([]byte(prefixUsers+":bl"), u.secondaryKeyReverse(u.Losses)...))
	batch.Delete(append([]byte(prefixUsers+":bp"), u.secondaryKeyReverse(u.Plays)...))
	batch.Delete(append([]byte(prefixUsers+":bu"), []byte(u.Name)...))

	return lds.db.Write(batch, nil)
}

func (lds *LevelDataStore) NumUsers() (int, error) {
	return 0, nil
}

func (lds *LevelDataStore) GetServers() ([]*Server, error) {
	servers := make([]*Server, 0, 16)
	iter := lds.db.NewIterator(nil)
	prefix := []byte(prefixServers)
	defer iter.Release()
	for hasNext := iter.Seek(prefix); hasNext && bytes.HasPrefix(iter.Key(), prefix); hasNext = iter.Next() {
		s := new(Server)
		lds.encoder.Unmarshal(iter.Value(), s)
		servers = append(servers, s)
	}
	return servers, nil
}
func (lds *LevelDataStore) GetServer(serverKey []byte) (*Server, error) {
	if value, err := lds.db.Get(serverKey, nil); err == nil {
		s := new(Server)
		lds.encoder.Unmarshal(value, s)
		return s, nil
	} else if err == util.ErrNotFound {
		return nil, ErrServerNotFound
	} else {
		return nil, err
	}
}
func (lds *LevelDataStore) PutServer(s *Server) error {
	if v, e := lds.encoder.Marshal(s); e == nil {
		return lds.db.Put(serverKey(*s), v, nil)
	} else {
		return e
	}
}
func (lds *LevelDataStore) DeleteServer(s *Server) error {
	return lds.db.Delete(serverKey(*s), nil)
}

func (lds *LevelDataStore) NumServers() (int, error) {
	return 0, nil
}

func (lds *LevelDataStore) Close() {
	lds.db.Close()
	if lds.isTemp {
		os.RemoveAll(lds.dbPath)
	}
}
