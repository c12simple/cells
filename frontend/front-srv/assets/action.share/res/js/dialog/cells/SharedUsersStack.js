/*
 * Copyright 2007-2017 Charles du Jeu - Abstrium SAS <team (at) pyd.io>
 * This file is part of Pydio.
 *
 * Pydio is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * Pydio is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with Pydio.  If not, see <http://www.gnu.org/licenses/>.
 *
 * The latest code can be found at <https://pydio.com>.
 */
import React from 'react'
import Avatar from "./Avatar";

const SharedUsersStack = ({acls, max=6, size=32}) => {

    let participantStyle = {
        border: '2px solid var(--md-sys-color-surface-variant)',
    }
    let participants = Object.keys(acls).map((k,i) => {
        let type, object;
        const acl = acls[k];
        if(acl.User) {
            type = 'user';
            object = acl.User
        }else if (acl.Group) {
            type = 'group'
            object = acl.Group;
        } else if(acl.Role) {
            type = 'team'
            object = acl.Role
        } else {
            return null
        }
        let style = {...participantStyle};
        if(i > 0) {
            style.marginLeft = -10;
            style.zIndex = i*2
        }
        return {type, object, style}
    }).filter(a => a !== null)

    if(participants.length > max) {
        const moreSize = participants.length - max
        participants = participants.slice(0, max)
        participants.push({type:'more', object:moreSize, style:{...participantStyle, marginLeft:-10, zIndex:16, fontWeight: 500, fontSize: 14}})
    }


    return (
        <div style={{display:'flex', height:size}}>
            {participants.map(({type, object, style})=> <Avatar size={size} type={type} idmObject={object} style={style} tooltip={true}/>)}
        </div>
    )

}

export default SharedUsersStack